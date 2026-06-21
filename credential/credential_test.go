package credential

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDynamo returns scripted responses for successive UpdateItem calls.
type fakeDynamo struct {
	responses []response
	calls     []*dynamodb.UpdateItemInput
}

type response struct {
	out *dynamodb.UpdateItemOutput
	err error
}

func (f *fakeDynamo) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.calls = append(f.calls, in)
	r := f.responses[len(f.calls)-1]
	return r.out, r.err
}

func item(enabled bool, counter, lastAccessed string) *dynamodb.UpdateItemOutput {
	attrs := map[string]types.AttributeValue{
		"key":            &types.AttributeValueMemberS{Value: "abc"},
		"credential":     &types.AttributeValueMemberS{Value: "secret-value"},
		"enabled":        &types.AttributeValueMemberBOOL{Value: enabled},
		"usageCounter":   &types.AttributeValueMemberN{Value: counter},
		"lastAccessedAt": &types.AttributeValueMemberS{Value: lastAccessed},
	}
	return &dynamodb.UpdateItemOutput{Attributes: attrs}
}

func TestGetEnabledIncrementsCounter(t *testing.T) {
	fake := &fakeDynamo{responses: []response{
		{out: item(true, "6", "2026-06-21T13:26:00.123Z")},
	}}
	s := &store{client: fake, tableName: "test-table"}

	got, err := s.get(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 UpdateItem call, got %d", len(fake.calls))
	}
	if got != "secret-value" {
		t.Errorf("credential = %q, want %q", got, "secret-value")
	}
	// Enabled-path call must set lastUsedAt and increment the counter.
	if expr := *fake.calls[0].UpdateExpression; expr != "SET lastAccessedAt = :now, lastUsedAt = :now ADD usageCounter :one" {
		t.Errorf("unexpected update expression: %q", expr)
	}
}

func TestGetDisabledReturnsNotFound(t *testing.T) {
	fake := &fakeDynamo{responses: []response{
		{err: &types.ConditionalCheckFailedException{}},
		{out: item(false, "10", "2026-06-21T13:26:00.123Z")},
	}}
	s := &store{client: fake, tableName: "test-table"}

	got, err := s.get(context.Background(), "abc")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got != "" {
		t.Errorf("credential = %q, want empty (withheld for disabled)", got)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 UpdateItem calls, got %d", len(fake.calls))
	}
	// Disabled-path call must still touch lastAccessedAt (and nothing else).
	if expr := *fake.calls[1].UpdateExpression; expr != "SET lastAccessedAt = :now" {
		t.Errorf("unexpected update expression: %q", expr)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	fake := &fakeDynamo{responses: []response{
		{err: &types.ConditionalCheckFailedException{}},
		{err: &types.ConditionalCheckFailedException{}},
	}}
	s := &store{client: fake, tableName: "test-table"}

	_, err := s.get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 UpdateItem calls, got %d", len(fake.calls))
	}
}

func TestGetPropagatesTransientError(t *testing.T) {
	boom := errors.New("throttled")
	fake := &fakeDynamo{responses: []response{
		{err: boom},
	}}
	s := &store{client: fake, tableName: "test-table"}

	_, err := s.get(context.Background(), "abc")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("transient error must not be reported as ErrNotFound")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 UpdateItem call, got %d", len(fake.calls))
	}
}
