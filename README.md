# ksw-credential-by-dynamodb

A small Go library that reads a credential document from a DynamoDB table
(configured via `TABLE_NAME`) by its partition key (`key`).

Each read also "touches" the item:

- `lastAccessedAt` is set to the current time (RFC3339 with milliseconds, UTC).
- `lastUsedAt` is set to the current time **only when** `enabled` is `true`.
- `usageCounter` is incremented by 1 **only when** `enabled` is `true`.

The update is done with a single atomic conditional `UpdateItem`, so the counter
is race-free under concurrent access.

## Install

```sh
go get github.com/octoplux/ksw-credential-by-dynamodb/credential
```

## Usage

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/octoplux/ksw-credential-by-dynamodb/credential"
)

func main() {
	ctx := context.Background()

	cred, err := credential.Get(ctx, "my-partition-key")
	if errors.Is(err, credential.ErrNotFound) {
		log.Fatal("no such credential")
	} else if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("credential=%s\n", cred)
}
```

`credential.Get` builds its DynamoDB client once, on the first call, from the AWS
environment variables below.

## Configuration

The library reads from the table named by `TABLE_NAME` and expects
these attributes:

| attribute        | type    |
|------------------|---------|
| `key`            | string (partition key) |
| `createdAt`      | string  |
| `credential`     | string  |
| `enabled`        | boolean |
| `lastAccessedAt` | string  |
| `lastUsedAt`     | string  |
| `usageCounter`   | number  |

`credential.Get` requires these environment variables to be set explicitly:

- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `AWS_REGION`
- `TABLE_NAME`

It returns an error if any of them is missing.

## Test

```sh
go test ./...
```
