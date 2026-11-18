package routing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLimitKeyQuery(t *testing.T) {
	var asked []string
	sharing := func(_ context.Context, userID string, others []string) ([]string, error) {
		require.Equal(t, "@alice:test", userID)
		asked = others
		return []string{"@bob:remote"}, nil
	}

	query := map[string][]string{"@alice:test": {}, "@bob:remote": {"DEV"}, "@carol:test": {}}
	require.NoError(t, limitKeyQuery(t.Context(), query, "@alice:test", sharing))
	require.Equal(t, map[string][]string{"@alice:test": {}, "@bob:remote": {"DEV"}}, query)
	require.ElementsMatch(t, []string{"@bob:remote", "@carol:test"}, asked, "only the other users are looked up")

	// A query for the requester alone needs no lookup.
	query = map[string][]string{"@alice:test": {}}
	require.NoError(t, limitKeyQuery(t.Context(), query, "@alice:test", func(context.Context, string, []string) ([]string, error) {
		t.Fatal("unexpected lookup")
		return nil, nil
	}))
	require.Len(t, query, 1)

	failing := func(context.Context, string, []string) ([]string, error) { return nil, errors.New("down") }
	require.Error(t, limitKeyQuery(t.Context(), map[string][]string{"@bob:remote": {}}, "@alice:test", failing))
}
