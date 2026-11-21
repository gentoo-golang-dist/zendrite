package shared_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/postgres"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func mustCreateRoomserverDatabase(t *testing.T, dbType test.DBType) (*shared.Database, func()) {
	t.Helper()

	connStr, clearDB := test.PrepareDBConnectionString(t, dbType)
	dbOpts := &config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	writer := sqlutil.NewExclusiveWriter()
	db, err := sqlutil.Open(dbOpts, writer)
	assert.NoError(t, err)

	var membershipTable tables.Membership
	var stateKeyTable tables.EventStateKeys
	var userRoomKeys tables.UserRoomKeys
	var roomsTable tables.Rooms
	switch dbType {
	case test.DBTypePostgres:
		err = postgres.CreateRoomsTable(db)
		assert.NoError(t, err)
		err = postgres.CreateEventStateKeysTable(db)
		assert.NoError(t, err)
		err = postgres.CreateMembershipTable(db)
		assert.NoError(t, err)
		err = postgres.CreatePublishedTable(db)
		assert.NoError(t, err)
		err = postgres.CreateUserRoomKeysTable(db)
		assert.NoError(t, err)
		err = postgres.CreateEventJSONTable(db)
		assert.NoError(t, err)
		roomsTable, err = postgres.PrepareRoomsTable(db)
		assert.NoError(t, err)
		membershipTable, err = postgres.PrepareMembershipTable(db)
		assert.NoError(t, err)
		stateKeyTable, err = postgres.PrepareEventStateKeysTable(db)
		assert.NoError(t, err)
		userRoomKeys, err = postgres.PrepareUserRoomKeysTable(db)
	case test.DBTypeSQLite:
		err = sqlite3.CreateRoomsTable(db)
		assert.NoError(t, err)
		err = sqlite3.CreateEventStateKeysTable(db)
		assert.NoError(t, err)
		err = sqlite3.CreateMembershipTable(db)
		assert.NoError(t, err)
		err = sqlite3.CreatePublishedTable(db)
		assert.NoError(t, err)
		err = sqlite3.CreateUserRoomKeysTable(db)
		assert.NoError(t, err)
		err = sqlite3.CreateEventJSONTable(db)
		assert.NoError(t, err)
		roomsTable, err = sqlite3.PrepareRoomsTable(db)
		assert.NoError(t, err)
		membershipTable, err = sqlite3.PrepareMembershipTable(db)
		assert.NoError(t, err)
		stateKeyTable, err = sqlite3.PrepareEventStateKeysTable(db)
		assert.NoError(t, err)
		userRoomKeys, err = sqlite3.PrepareUserRoomKeysTable(db)
	}
	assert.NoError(t, err)

	cache := caching.NewRistrettoCache(8*1024*1024, time.Hour, false)

	evDb := shared.EventDatabase{EventStateKeysTable: stateKeyTable, Cache: cache, Writer: writer}

	database := &shared.Database{
		DB:               db,
		EventDatabase:    evDb,
		MembershipTable:  membershipTable,
		UserRoomKeyTable: userRoomKeys,
		RoomsTable:       roomsTable,
		Writer:           writer,
		Cache:            cache,
	}
	return database, func() {
		clearDB()
		err = db.Close()
		assert.NoError(t, err)
	}
}

func Test_GetLeftUsers(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)

	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateRoomserverDatabase(t, dbType) //nolint:contextcheck
		defer close()

		// Create dummy entries
		for _, user := range []*test.User{alice, bob, charlie} {
			nid, err := db.EventStateKeysTable.InsertEventStateKeyNID(ctx, nil, user.ID)
			assert.NoError(t, err)
			err = db.MembershipTable.InsertMembership(ctx, nil, 1, nid, true)
			assert.NoError(t, err)
			// We must update the membership with a non-zero event NID or it will get filtered out in later queries
			membershipNID := tables.MembershipStateLeaveOrBan
			if user == alice {
				membershipNID = tables.MembershipStateJoin
			}
			_, err = db.MembershipTable.UpdateMembership(ctx, nil, 1, nid, nid, membershipNID, 1, false)
			assert.NoError(t, err)
		}

		// Now try to get the left users, this should be Bob and Charlie, since they have a "leave" membership
		expectedUserIDs := []string{bob.ID, charlie.ID}
		leftUsers, err := db.GetLeftUsers(context.Background(), []string{alice.ID, bob.ID, charlie.ID}) //nolint:contextcheck
		assert.NoError(t, err)
		assert.ElementsMatch(t, expectedUserIDs, leftUsers)
	})
}

func Test_GetKnownUsersWithoutMemberships(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateRoomserverDatabase(t, dbType) //nolint:contextcheck
		defer close()

		roomNID, err := db.RoomsTable.InsertRoomNID(ctx, nil, "!public:test", "10")
		assert.NoError(t, err)
		nid, err := db.EventStateKeysTable.InsertEventStateKeyNID(ctx, nil, alice.ID)
		assert.NoError(t, err)
		assert.NoError(t, db.MembershipTable.InsertMembership(ctx, nil, roomNID, nid, true))
		_, err = db.MembershipTable.UpdateMembership(ctx, nil, roomNID, nid, nid, tables.MembershipStateJoin, 1, false)
		assert.NoError(t, err)

		// Bob has never been in a room, so he has no event state key NID.
		knownUsers, err := db.GetKnownUsers(ctx, bob.ID, "", 10)
		assert.NoError(t, err)
		assert.Empty(t, knownUsers)

		// Once the room is published, Bob can see its members.
		_, err = db.DB.ExecContext(ctx, "INSERT INTO roomserver_published (room_id, appservice_id, network_id, published) VALUES ('!public:test', '', '', true)")
		assert.NoError(t, err)
		knownUsers, err = db.GetKnownUsers(ctx, bob.ID, "", 10)
		assert.NoError(t, err)
		assert.Equal(t, []string{alice.ID}, knownUsers)
	})
}

// MSC4263: key queries are answered for the users who have any membership
// in a room the requester is joined or invited to, or in a published room.
func TestUsersSharingRoom(t *testing.T) {
	ctx := context.Background()
	users := map[string]*test.User{}
	for _, name := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "gina", "harry", "nobody"} {
		users[name] = test.NewUser(t)
	}
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateRoomserverDatabase(t, dbType) //nolint:contextcheck
		defer close()

		nids := map[string]types.EventStateKeyNID{}
		for _, name := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "gina", "harry"} {
			nid, err := db.EventStateKeysTable.InsertEventStateKeyNID(ctx, nil, users[name].ID)
			assert.NoError(t, err)
			nids[name] = nid
		}
		rooms := map[string]map[string]tables.MembershipState{
			// Alice is joined: everyone with a membership shares it.
			"!shared:test": {"alice": tables.MembershipStateJoin, "bob": tables.MembershipStateInvite, "carol": tables.MembershipStateLeaveOrBan},
			// Alice is only invited.
			"!invited:test": {"alice": tables.MembershipStateInvite, "erin": tables.MembershipStateJoin},
			// Alice left.
			"!left:test": {"alice": tables.MembershipStateLeaveOrBan, "gina": tables.MembershipStateJoin},
			// Alice was never in it, but it gets published.
			"!public:test":  {"frank": tables.MembershipStateJoin},
			"!private:test": {"dave": tables.MembershipStateJoin},
		}
		for roomID, members := range rooms {
			roomNID, err := db.RoomsTable.InsertRoomNID(ctx, nil, roomID, "10")
			assert.NoError(t, err)
			for name, membership := range members {
				assert.NoError(t, db.MembershipTable.InsertMembership(ctx, nil, roomNID, nids[name], true))
				_, err = db.MembershipTable.UpdateMembership(ctx, nil, roomNID, nids[name], nids[name], membership, 1, false)
				assert.NoError(t, err)
			}
		}
		// Harry invited Alice over federation: only her invite is known.
		roomNID, err := db.RoomsTable.InsertRoomNID(ctx, nil, "!fedinvite:test", "10")
		assert.NoError(t, err)
		assert.NoError(t, db.MembershipTable.InsertMembership(ctx, nil, roomNID, nids["alice"], true))
		_, err = db.MembershipTable.UpdateMembership(ctx, nil, roomNID, nids["alice"], nids["harry"], tables.MembershipStateInvite, 1, false)
		assert.NoError(t, err)

		query := func(name string) []string {
			others := make([]string, 0, len(users))
			for other, user := range users {
				if other != name {
					others = append(others, user.ID)
				}
			}
			sharing, err := db.UsersSharingRoom(ctx, users[name].ID, others)
			assert.NoError(t, err)
			return sharing
		}

		assert.ElementsMatch(t, []string{users["bob"].ID, users["carol"].ID, users["erin"].ID, users["harry"].ID}, query("alice"))
		// Nobody has never been in a room.
		assert.Empty(t, query("nobody"))

		// Lookups are split into batches.
		many := []string{users["bob"].ID}
		for i := range 1200 {
			many = append(many, fmt.Sprintf("@unknown%d:test", i))
		}
		many = append(many, users["erin"].ID)
		sharing, err := db.UsersSharingRoom(ctx, users["alice"].ID, many)
		assert.NoError(t, err)
		assert.ElementsMatch(t, []string{users["bob"].ID, users["erin"].ID}, sharing)

		_, err = db.DB.ExecContext(ctx, "INSERT INTO roomserver_published (room_id, appservice_id, network_id, published) VALUES ('!public:test', '', '', true)")
		assert.NoError(t, err)
		assert.ElementsMatch(t, []string{users["bob"].ID, users["carol"].ID, users["erin"].ID, users["frank"].ID, users["harry"].ID}, query("alice"))
		assert.Equal(t, []string{users["frank"].ID}, query("nobody"))

		// Memberships in pseudo ID rooms cannot be matched, so a user in one
		// sees everyone.
		_, err = db.UserRoomKeyTable.InsertUserRoomPrivatePublicKey(ctx, nil, nids["dave"], roomNID, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
		assert.NoError(t, err)
		assert.Len(t, query("dave"), len(users)-1)
	})
}

func TestUserRoomKeys(t *testing.T) {
	ctx := context.Background()
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)

	userID, err := spec.NewUserID(alice.ID, true)
	assert.NoError(t, err)
	roomID, err := spec.NewRoomID(room.ID)
	assert.NoError(t, err)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateRoomserverDatabase(t, dbType) //nolint:contextcheck
		defer close()

		// create a room NID so we can query the room
		_, err := db.RoomsTable.InsertRoomNID(ctx, nil, roomID.String(), gomatrixserverlib.RoomVersionV10)
		assert.NoError(t, err)
		doesNotExist, err := spec.NewRoomID("!doesnotexist:localhost")
		assert.NoError(t, err)
		_, err = db.RoomsTable.InsertRoomNID(ctx, nil, doesNotExist.String(), gomatrixserverlib.RoomVersionV10)
		assert.NoError(t, err)

		_, key, err := ed25519.GenerateKey(nil)
		assert.NoError(t, err)

		gotKey, err := db.InsertUserRoomPrivatePublicKey(ctx, *userID, *roomID, key)
		assert.NoError(t, err)
		assert.Equal(t, gotKey, key)

		// again, this shouldn't result in an error, but return the existing key
		_, key2, err := ed25519.GenerateKey(nil)
		assert.NoError(t, err)
		gotKey, err = db.InsertUserRoomPrivatePublicKey(ctx, *userID, *roomID, key2)
		assert.NoError(t, err)
		assert.Equal(t, gotKey, key)

		gotKey, err = db.SelectUserRoomPrivateKey(context.Background(), *userID, *roomID) //nolint:contextcheck
		assert.NoError(t, err)
		assert.Equal(t, key, gotKey)
		pubKey, err := db.SelectUserRoomPublicKey(context.Background(), *userID, *roomID) //nolint:contextcheck
		assert.NoError(t, err)
		assert.Equal(t, key.Public(), pubKey)

		// Key doesn't exist, we shouldn't get anything back
		gotKey, err = db.SelectUserRoomPrivateKey(context.Background(), *userID, *doesNotExist) //nolint:contextcheck
		assert.NoError(t, err)
		assert.Nil(t, gotKey)
		pubKey, err = db.SelectUserRoomPublicKey(context.Background(), *userID, *doesNotExist) //nolint:contextcheck
		assert.NoError(t, err)
		assert.Nil(t, pubKey)

		pubKeyForQuery, ok := key.Public().(ed25519.PublicKey)
		assert.True(t, ok, "key.Public() should be ed25519.PublicKey")
		queryUserIDs := map[spec.RoomID][]ed25519.PublicKey{
			*roomID: {pubKeyForQuery},
		}

		userIDs, err := db.SelectUserIDsForPublicKeys(ctx, queryUserIDs)
		assert.NoError(t, err)
		pubKeyForWant, ok := key.Public().(ed25519.PublicKey)
		assert.True(t, ok, "key.Public() should be ed25519.PublicKey")
		wantKeys := map[spec.RoomID]map[string]string{
			*roomID: {
				spec.Base64Bytes(pubKeyForWant).Encode(): userID.String(),
			},
		}
		assert.Equal(t, wantKeys, userIDs)

		// insert key that came in over federation
		var gotPublicKey, key4 ed25519.PublicKey
		key4, _, err = ed25519.GenerateKey(nil)
		assert.NoError(t, err)
		gotPublicKey, err = db.InsertUserRoomPublicKey(context.Background(), *userID, *doesNotExist, key4) //nolint:contextcheck
		assert.NoError(t, err)
		assert.Equal(t, key4, gotPublicKey)

		// test invalid room
		reallyDoesNotExist, err := spec.NewRoomID("!reallydoesnotexist:localhost")
		assert.NoError(t, err)
		_, err = db.InsertUserRoomPublicKey(context.Background(), *userID, *reallyDoesNotExist, key4) //nolint:contextcheck
		assert.Error(t, err)
		_, err = db.InsertUserRoomPrivatePublicKey(context.Background(), *userID, *reallyDoesNotExist, key) //nolint:contextcheck
		assert.Error(t, err)
	})
}

func TestAssignRoomNID(t *testing.T) {
	ctx := context.Background()
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)

	roomID, err := spec.NewRoomID(room.ID)
	assert.NoError(t, err)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateRoomserverDatabase(t, dbType) //nolint:contextcheck
		defer close()

		nid, err := db.AssignRoomNID(ctx, *roomID, room.Version)
		assert.NoError(t, err)
		assert.Greater(t, nid, types.EventNID(0))

		_, err = db.AssignRoomNID(ctx, spec.RoomID{}, "notaroomversion")
		assert.Error(t, err)
	})
}
