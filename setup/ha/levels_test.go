package ha_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/ha"
)

// TestLevelsKeepRollingUpgrades refuses an upgrade level that would turn an
// upgrade within one protocol into an offline upgrade.
//
// Deployments update unattended and may skip releases, so every build of a
// protocol must be able to take over from every other build of that protocol,
// in both directions. That holds while UpgradeFrom and RollbackTo stay at the
// first level of the protocol. A change that no rolling upgrade can carry, and
// that can't be split into backward compatible levels, starts a new protocol
// instead, and its release is a breaking change.
func TestLevelsKeepRollingUpgrades(t *testing.T) {
	data, err := os.ReadFile(levelsFile)
	require.NoError(t, err)
	var levels []level
	require.NoError(t, json.Unmarshal(data, &levels))
	require.NotEmpty(t, levels)

	first := map[int]int{}
	for i, l := range levels {
		if i > 0 {
			previous := levels[i-1]
			require.Greater(t, l.Level, previous.Level, "%s must list levels in ascending order", levelsFile)
			require.GreaterOrEqual(t, l.Protocol, previous.Protocol, "%s must not return to an older protocol at level %d", levelsFile, l.Level)
		}
		if _, ok := first[l.Protocol]; !ok {
			first[l.Protocol] = l.Level
		}
		requireRolling(t, l.Contract, first[l.Protocol])
	}

	release := ha.Release()
	if base, ok := first[release.Protocol]; ok {
		requireRolling(t, release, base)
	}
}

func requireRolling(t *testing.T, c ha.Contract, base int) {
	t.Helper()
	hint := "split the change into backward compatible levels, or start a new protocol in a breaking release " +
		"(see Changing shared state in website/src/content/docs/development/contributing.md)"
	require.LessOrEqual(t, c.UpgradeFrom, base,
		"level %d raises upgrade_from to %d above level %d, the first of protocol %d, which makes upgrades to it offline; %s",
		c.Level, c.UpgradeFrom, base, c.Protocol, hint)
	require.LessOrEqual(t, c.RollbackTo, base,
		"level %d raises rollback_to to %d above level %d, the first of protocol %d, which makes rollbacks from it offline; %s",
		c.Level, c.RollbackTo, base, c.Protocol, hint)
}
