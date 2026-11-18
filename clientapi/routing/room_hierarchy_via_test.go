package routing

import (
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHierarchyVias(t *testing.T) {
	query, err := url.ParseQuery("via=a.example&org.matrix.msc4235.via=b.example&via=a.example&via=&via=bad/path&via=a.example:99999")
	require.NoError(t, err)
	require.Equal(t, []string{"a.example", "b.example"}, hierarchyVias(query))
	require.Empty(t, hierarchyVias(url.Values{}))

	many := url.Values{}
	for i := range maxHierarchyVias + 5 {
		many.Add("via", strconv.Itoa(i)+".example")
	}
	require.Len(t, hierarchyVias(many), maxHierarchyVias)
}
