package config

import (
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"
	log "github.com/sirupsen/logrus"
)

type RoomServer struct {
	Matrix *Global `yaml:"-"`

	DefaultRoomVersion gomatrixserverlib.RoomVersion `yaml:"default_room_version,omitempty"`

	// AutoPurgeEmptyRooms enables automatic purging of empty rooms.
	// When true (the default), two behaviors fire together:
	//   - at startup, a one-shot sweep purges any rooms which currently have
	//     no local members; and
	//   - at runtime, whenever the last local member leaves a room, the room
	//     is scheduled for an asynchronous purge.
	// Set to false to disable both behaviors.
	AutoPurgeEmptyRooms bool `yaml:"auto_purge_empty_rooms"`

	// AutoForgetOnLeave makes every transition to leave or ban for a local
	// user behave as if the user had also called /forget on the room. This
	// matches the Matrix 1.18 m.forget_forced_upon_leave capability, which
	// is advertised via /_matrix/client/v3/capabilities when this is true.
	// Independent of AutoPurgeEmptyRooms: a forgotten room is not deleted,
	// just hidden from the leaving user's history.
	//
	// Defaults to false. The asymmetric trade-off is the reason: forgetting
	// a room drops the user's archived-rooms UI entry and their /messages
	// access to pre-leave history, which is worse for users who got kicked
	// or who left a high-activity room. Users who prefer the auto-forget UX
	// can opt in.
	AutoForgetOnLeave bool `yaml:"auto_forget_on_leave"`

	Database DatabaseOptions `yaml:"database,omitempty"`
}

func (c *RoomServer) Defaults(opts DefaultOpts) {
	c.DefaultRoomVersion = gomatrixserverlib.RoomVersionV12
	c.AutoPurgeEmptyRooms = true
	if opts.Generate {
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:roomserver.db"
		}
	}
}

func (c *RoomServer) Verify(configErrs *ConfigErrors) {
	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "room_server.database.connection_string", string(c.Database.ConnectionString))
	}

	if !gomatrixserverlib.KnownRoomVersion(c.DefaultRoomVersion) {
		configErrs.Add(fmt.Sprintf("invalid value for config key 'room_server.default_room_version': unsupported room version: %q", c.DefaultRoomVersion))
	} else if !gomatrixserverlib.StableRoomVersion(c.DefaultRoomVersion) {
		log.Warnf("WARNING: Provided default room version %q is unstable", c.DefaultRoomVersion)
	}
}
