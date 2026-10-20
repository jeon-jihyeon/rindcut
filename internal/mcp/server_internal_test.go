package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
)

func TestAddInvalidOutput(t *testing.T) {
	t.Parallel()
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "test"}, nil)
	assert.Panics(t, func() {
		add(srv, "invalid-output", "invalid declaration", func(context.Context, Empty) (chan int, error) {
			return nil, nil
		})
	})
}
