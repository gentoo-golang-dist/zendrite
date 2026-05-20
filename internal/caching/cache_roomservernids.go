package caching

import (
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

type RoomServerCaches interface {
	RoomServerNIDsCache
	RoomVersionCache
	RoomServerEventsCache
	RoomHierarchyCache
	EventStateKeyCache
	EventTypeCache
}

// RoomServerNIDsCache contains the subset of functions needed for
// a roomserver NID cache.
type RoomServerNIDsCache interface {
	GetRoomServerRoomID(roomNID types.RoomNID) (string, bool)
	// StoreRoomServerRoomID stores roomNID -> roomID and roomID -> roomNID
	StoreRoomServerRoomID(roomNID types.RoomNID, roomID string)
	GetRoomServerRoomNID(roomID string) (types.RoomNID, bool)
	// InvalidateRoom removes all room-keyed cache entries for the given room.
	// Call this after the room has been purged from the database, so that
	// subsequent lookups by roomID (e.g. after a rejoin) don't hit stale data.
	InvalidateRoom(roomID string, roomNID types.RoomNID)
}

func (c Caches) GetRoomServerRoomID(roomNID types.RoomNID) (string, bool) {
	return c.RoomServerRoomIDs.Get(roomNID)
}

// StoreRoomServerRoomID stores roomNID -> roomID and roomID -> roomNID.
func (c Caches) StoreRoomServerRoomID(roomNID types.RoomNID, roomID string) {
	c.RoomServerRoomNIDs.Set(roomID, roomNID)
	c.RoomServerRoomIDs.Set(roomNID, roomID)
}

func (c Caches) GetRoomServerRoomNID(roomID string) (types.RoomNID, bool) {
	return c.RoomServerRoomNIDs.Get(roomID)
}

// InvalidateRoom unsets the room-ID and roomNID keyed cache entries for a room.
// NID-keyed event/state-key/event-type caches are left to expire via TTL: their
// NIDs are unique per row and will not be referenced again after a purge.
func (c Caches) InvalidateRoom(roomID string, roomNID types.RoomNID) {
	c.RoomServerRoomNIDs.Unset(roomID)
	c.RoomServerRoomIDs.Unset(roomNID)
	c.RoomVersions.Unset(roomID)
	c.RoomHierarchies.Unset(roomID)
	c.RoomHierarchyFailures.Unset(roomID)
	c.InvalidateRoomSummary(roomID)
}
