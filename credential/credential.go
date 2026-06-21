// Package credential reads credential documents from a DynamoDB table (named by
// the TABLE_NAME environment variable) by partition key. The credential value
// is returned only when the item is enabled; a missing or disabled credential
// is reported as ErrNotFound. Each read is also a "touch": it records the access
// time and, for enabled credentials, also records the last-used time and
// increments a usage counter.
package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// version is the semantic version of this library.
const version = "1.0.1"

// timeLayout is RFC3339 with millisecond precision, in UTC.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// ErrNotFound is returned when no item exists for the given partition key.
var ErrNotFound = errors.New("credential not found")

// credentialItem mirrors one item in the table. The dynamodbav tags
// map struct fields to the table's attribute names.
type credentialItem struct {
	Key            string `dynamodbav:"key"`
	CreatedAt      string `dynamodbav:"createdAt"`
	Credential     string `dynamodbav:"credential"`
	Enabled        bool   `dynamodbav:"enabled"`
	LastAccessedAt string `dynamodbav:"lastAccessedAt"`
	LastUsedAt     string `dynamodbav:"lastUsedAt"`
	UsageCounter   int64  `dynamodbav:"usageCounter"`
}

// dynamoAPI is the subset of the DynamoDB client used by store. It lets tests
// inject a fake; the concrete *dynamodb.Client satisfies it.
type dynamoAPI interface {
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// store reads credentials from a single DynamoDB table.
type store struct {
	client    dynamoAPI
	tableName string
}

var (
	defaultStore *store
	storeOnce    sync.Once
	storeErr     error
)

// Get looks up the credential for key, setting lastAccessedAt to the current
// time and, when the credential is enabled, also setting lastUsedAt and
// incrementing usageCounter. It returns the credential value only when the item
// is enabled; if no item exists for the key or the credential is disabled, it
// returns ErrNotFound.
//
// The DynamoDB client is built once, on the first call, from the
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and AWS_REGION environment
// variables; Get returns an error if any of them is unset.
func Get(ctx context.Context, key string) (string, error) {
	storeOnce.Do(func() { defaultStore, storeErr = newStore(ctx) })
	if storeErr != nil {
		return "", storeErr
	}
	return defaultStore.get(ctx, key)
}

// newStore builds a store from explicit environment variables:
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION, and
// TABLE_NAME (the DynamoDB table to read from). It returns an error if
// any of them is unset.
func newStore(ctx context.Context) (*store, error) {
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	region := os.Getenv("AWS_REGION")
	table := os.Getenv("TABLE_NAME")

	switch {
	case accessKeyID == "":
		return nil, errors.New("AWS_ACCESS_KEY_ID is not set")
	case secretAccessKey == "":
		return nil, errors.New("AWS_SECRET_ACCESS_KEY is not set")
	case region == "":
		return nil, errors.New("AWS_REGION is not set")
	case table == "":
		return nil, errors.New("TABLE_NAME is not set")
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &store{client: dynamodb.NewFromConfig(cfg), tableName: table}, nil
}

// get fetches the credential identified by key. As a side effect it sets
// lastAccessedAt to the current time and, when the credential is enabled, also
// sets lastUsedAt and increments usageCounter by 1. It returns the credential
// value only when the item is enabled; a missing or disabled credential is
// reported as ErrNotFound.
func (s *store) get(ctx context.Context, key string) (string, error) {
	now := time.Now().UTC().Format(timeLayout)
	keyAttr := map[string]types.AttributeValue{
		"key": &types.AttributeValueMemberS{Value: key},
	}

	// Attempt 1 — enabled path: touch lastAccessedAt and lastUsedAt and bump
	// usageCounter, guarded so it only applies to an existing, enabled item.
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.tableName),
		Key:                 keyAttr,
		UpdateExpression:    aws.String("SET lastAccessedAt = :now, lastUsedAt = :now ADD usageCounter :one"),
		ConditionExpression: aws.String("attribute_exists(#k) AND #en = :true"),
		ExpressionAttributeNames: map[string]string{
			"#k":  "key",
			"#en": "enabled",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now":  &types.AttributeValueMemberS{Value: now},
			":one":  &types.AttributeValueMemberN{Value: "1"},
			":true": &types.AttributeValueMemberBOOL{Value: true},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err == nil {
		return unmarshal(out.Attributes)
	}

	// The item is either missing or disabled; both fail the condition above.
	var condErr *types.ConditionalCheckFailedException
	if !errors.As(err, &condErr) {
		return "", fmt.Errorf("update credential %q: %w", key, err)
	}

	// Attempt 2 — disabled path: touch lastAccessedAt only, still requiring the
	// item to exist so a missing key is reported as ErrNotFound. A disabled
	// credential is withheld and reported as ErrNotFound too, so the value is
	// returned only when the credential is enabled (Attempt 1).
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.tableName),
		Key:                 keyAttr,
		UpdateExpression:    aws.String("SET lastAccessedAt = :now"),
		ConditionExpression: aws.String("attribute_exists(#k)"),
		ExpressionAttributeNames: map[string]string{
			"#k": "key",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberS{Value: now},
		},
	})
	if err == nil {
		// Item exists but is disabled: access recorded, credential withheld.
		return "", ErrNotFound
	}
	if errors.As(err, &condErr) {
		return "", ErrNotFound
	}
	return "", fmt.Errorf("update credential %q: %w", key, err)
}

func unmarshal(attrs map[string]types.AttributeValue) (string, error) {
	var c credentialItem
	if err := attributevalue.UnmarshalMap(attrs, &c); err != nil {
		return "", fmt.Errorf("unmarshal credential: %w", err)
	}
	return c.Credential, nil
}
