package api

import (
	"context"
	"fmt"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/gomatrix"

	"codefloe.com/pat-s/zendrite/federationapi/types"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
)

// FederationInternalAPI is used to query information from the federation sender.
type FederationInternalAPI interface {
	gomatrixserverlib.FederatedStateClient
	gomatrixserverlib.FederatedJoinClient
	KeyserverFederationAPI
	gomatrixserverlib.KeyDatabase
	ClientFederationAPI
	RoomserverFederationAPI
	P2PFederationAPI

	QueryServerKeys(ctx context.Context, request *QueryServerKeysRequest, response *QueryServerKeysResponse) error
	LookupServerKeys(ctx context.Context, s spec.ServerName, keyRequests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp) ([]gomatrixserverlib.ServerKeys, error)
	MSC2836EventRelationships(ctx context.Context, origin, dst spec.ServerName, r fclient.MSC2836EventRelationshipsRequest, roomVersion gomatrixserverlib.RoomVersion) (res fclient.MSC2836EventRelationshipsResponse, err error)

	// Broadcasts an EDU to all servers in rooms we are joined to. Used in the yggdrasil demos.
	PerformBroadcastEDU(
		ctx context.Context,
		request *PerformBroadcastEDURequest,
		response *PerformBroadcastEDUResponse,
	) error
	PerformWakeupServers(
		ctx context.Context,
		request *PerformWakeupServersRequest,
		response *PerformWakeupServersResponse,
	) error
}

type ClientFederationAPI interface {
	// Query the server names of the joined hosts in a room.
	// Unlike QueryJoinedHostsInRoom, this function returns a de-duplicated slice
	// containing only the server names (without information for membership events).
	// The response will include this server if they are joined to the room.
	QueryJoinedHostServerNamesInRoom(ctx context.Context, request *QueryJoinedHostServerNamesInRoomRequest, response *QueryJoinedHostServerNamesInRoomResponse) error
}

type RoomserverFederationAPI interface {
	gomatrixserverlib.BackfillClient
	gomatrixserverlib.FederatedStateClient
	KeyRing() *gomatrixserverlib.KeyRing
	// IsServerBackingOff returns true if the server is blacklisted or currently backing off.
	IsServerBackingOff(serverName spec.ServerName) bool
	// PolicyServerSign asks a room's policy server to sign an event
	// (MSC4284) and returns the signatures from its response. A
	// *PolicyServerRefusal error means the policy server refused; any other
	// error means it could not be asked.
	PolicyServerSign(ctx context.Context, origin, policyServer spec.ServerName, event gomatrixserverlib.PDU) (map[string]map[string]spec.Base64Bytes, error)

	// PerformDirectoryLookup looks up a remote room ID from a room alias.
	PerformDirectoryLookup(ctx context.Context, request *PerformDirectoryLookupRequest, response *PerformDirectoryLookupResponse) error
	// Handle an instruction to make_join & send_join with a remote server.
	PerformJoin(ctx context.Context, request *PerformJoinRequest, response *PerformJoinResponse)
	// Handle an instruction to make_leave & send_leave with a remote server.
	PerformLeave(ctx context.Context, request *PerformLeaveRequest, response *PerformLeaveResponse) error
	// Handle an instruction to make_knock & send_knock with a remote server.
	PerformKnock(ctx context.Context, request *PerformKnockRequest, response *PerformKnockResponse) error
	// Handle sending an invite to a remote server.
	SendInvite(ctx context.Context, event gomatrixserverlib.PDU, strippedState []gomatrixserverlib.InviteStrippedState) (gomatrixserverlib.PDU, error)
	// Handle sending an invite to a remote server.
	SendInviteV3(ctx context.Context, event gomatrixserverlib.ProtoEvent, invitee spec.UserID, version gomatrixserverlib.RoomVersion, strippedState []gomatrixserverlib.InviteStrippedState) (gomatrixserverlib.PDU, error)
	// Handle an instruction to peek a room on a remote server.
	PerformOutboundPeek(ctx context.Context, request *PerformOutboundPeekRequest, response *PerformOutboundPeekResponse) error
	// Query the server names of the joined hosts in a room.
	// Unlike QueryJoinedHostsInRoom, this function returns a de-duplicated slice
	// containing only the server names (without information for membership events).
	// The response will include this server if they are joined to the room.
	QueryJoinedHostServerNamesInRoom(ctx context.Context, request *QueryJoinedHostServerNamesInRoomRequest, response *QueryJoinedHostServerNamesInRoomResponse) error
	GetEventAuth(ctx context.Context, origin, s spec.ServerName, roomVersion gomatrixserverlib.RoomVersion, roomID, eventID string) (res fclient.RespEventAuth, err error)
	GetEvent(ctx context.Context, origin, s spec.ServerName, eventID string) (res gomatrixserverlib.Transaction, err error)
	LookupMissingEvents(ctx context.Context, origin, s spec.ServerName, roomID string, missing fclient.MissingEvents, roomVersion gomatrixserverlib.RoomVersion) (res fclient.RespMissingEvents, err error)

	RoomHierarchies(ctx context.Context, origin, dst spec.ServerName, roomID string, suggestedOnly bool) (res fclient.RoomHierarchyResponse, err error)
}

type P2PFederationAPI interface {
	// Get the relay servers associated for the given server.
	P2PQueryRelayServers(
		ctx context.Context,
		request *P2PQueryRelayServersRequest,
		response *P2PQueryRelayServersResponse,
	) error

	// Add relay server associations to the given server.
	P2PAddRelayServers(
		ctx context.Context,
		request *P2PAddRelayServersRequest,
		response *P2PAddRelayServersResponse,
	) error

	// Remove relay server associations from the given server.
	P2PRemoveRelayServers(
		ctx context.Context,
		request *P2PRemoveRelayServersRequest,
		response *P2PRemoveRelayServersResponse,
	) error
}

// KeyserverFederationAPI is a subset of gomatrixserverlib.FederationClient functions which the keyserver
// implements as proxy calls, with built-in backoff/retries/etc. Errors returned from functions in
// this interface are of type FederationClientError.
type KeyserverFederationAPI interface {
	GetUserDevices(ctx context.Context, origin, s spec.ServerName, userID string) (res fclient.RespUserDevices, err error)
	ClaimKeys(ctx context.Context, origin, s spec.ServerName, oneTimeKeys map[string]map[string]string) (res fclient.RespClaimKeys, err error)
	QueryKeys(ctx context.Context, origin, s spec.ServerName, keys map[string][]string) (res fclient.RespQueryKeys, err error)
}

// FederationClientError is returned from FederationClient methods in the event of a problem.
type FederationClientError struct {
	Err         string
	RetryAfter  time.Duration
	Blacklisted bool
	Code        int // HTTP Status code from the remote server
}

func (e FederationClientError) Error() string {
	return fmt.Sprintf("%s - (retry_after=%s, blacklisted=%v)", e.Err, e.RetryAfter.String(), e.Blacklisted)
}

type QueryServerKeysRequest struct {
	ServerName      spec.ServerName
	KeyIDToCriteria map[gomatrixserverlib.KeyID]gomatrixserverlib.PublicKeyNotaryQueryCriteria
}

func (q *QueryServerKeysRequest) KeyIDs() []gomatrixserverlib.KeyID {
	kids := make([]gomatrixserverlib.KeyID, len(q.KeyIDToCriteria))
	i := 0
	for keyID := range q.KeyIDToCriteria {
		kids[i] = keyID
		i++
	}
	return kids
}

type QueryServerKeysResponse struct {
	ServerKeys []gomatrixserverlib.ServerKeys
}

type QueryPublicKeysRequest struct {
	Requests map[gomatrixserverlib.PublicKeyLookupRequest]spec.Timestamp `json:"requests"`
}

type QueryPublicKeysResponse struct {
	Results map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult `json:"results"`
}

type PerformDirectoryLookupRequest struct {
	RoomAlias  string          `json:"room_alias"`
	ServerName spec.ServerName `json:"server_name"`
}

type PerformDirectoryLookupResponse struct {
	RoomID      string            `json:"room_id"`
	ServerNames []spec.ServerName `json:"server_names"`
}

type PerformJoinRequest struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id"`
	// The sorted list of servers to try. Servers will be tried sequentially, after de-duplication.
	ServerNames types.ServerNames `json:"server_names"`
	Content     map[string]any    `json:"content"`
	Unsigned    map[string]any    `json:"unsigned"`
}

type PerformJoinResponse struct {
	JoinedVia spec.ServerName
	LastError *gomatrix.HTTPError
}

type PerformOutboundPeekRequest struct {
	RoomID string `json:"room_id"`
	// The sorted list of servers to try. Servers will be tried sequentially, after de-duplication.
	ServerNames types.ServerNames `json:"server_names"`
}

type PerformOutboundPeekResponse struct {
	LastError *gomatrix.HTTPError
}

type PerformLeaveRequest struct {
	RoomID      string            `json:"room_id"`
	UserID      string            `json:"user_id"`
	ServerNames types.ServerNames `json:"server_names"`
	Reason      string            `json:"reason,omitempty"`
}

type PerformLeaveResponse struct {
	// EventID is the ID of the leave event sent to the remote server.
	EventID string `json:"event_id,omitempty"`
}

type PerformKnockRequest struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id"`
	// The sorted list of servers to try. Servers will be tried sequentially, after de-duplication.
	ServerNames types.ServerNames `json:"server_names"`
	Content     map[string]any    `json:"content"`
}

type PerformKnockResponse struct {
	// The knock event as accepted by the remote server.
	Event gomatrixserverlib.PDU
	// The room state returned by the remote server, reduced to the entries
	// which pass MSC4311 validation.
	KnockRoomState []gomatrixserverlib.InviteStrippedState
	KnockedVia     spec.ServerName
	LastError      *gomatrix.HTTPError
}

type PerformInviteRequest struct {
	RoomVersion     gomatrixserverlib.RoomVersion           `json:"room_version"`
	Event           *rstypes.HeaderedEvent                  `json:"event"`
	InviteRoomState []gomatrixserverlib.InviteStrippedState `json:"invite_room_state"`
}

type PerformInviteResponse struct {
	Event *rstypes.HeaderedEvent `json:"event"`
}

// QueryJoinedHostServerNamesInRoomRequest is a request to QueryJoinedHostServerNames.
type QueryJoinedHostServerNamesInRoomRequest struct {
	RoomID             string `json:"room_id"`
	ExcludeSelf        bool   `json:"exclude_self"`
	ExcludeBlacklisted bool   `json:"exclude_blacklisted"`
}

// QueryJoinedHostServerNamesInRoomResponse is a response to QueryJoinedHostServerNames.
type QueryJoinedHostServerNamesInRoomResponse struct {
	ServerNames []spec.ServerName `json:"server_names"`
}

type PerformBroadcastEDURequest struct{}

type PerformBroadcastEDUResponse struct{}

type PerformWakeupServersRequest struct {
	ServerNames []spec.ServerName `json:"server_names"`
}

type PerformWakeupServersResponse struct{}

type InputPublicKeysRequest struct {
	Keys map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult `json:"keys"`
}

type InputPublicKeysResponse struct{}

type P2PQueryRelayServersRequest struct {
	Server spec.ServerName
}

type P2PQueryRelayServersResponse struct {
	RelayServers []spec.ServerName
}

type P2PAddRelayServersRequest struct {
	Server       spec.ServerName
	RelayServers []spec.ServerName
}

type P2PAddRelayServersResponse struct{}

type P2PRemoveRelayServersRequest struct {
	Server       spec.ServerName
	RelayServers []spec.ServerName
}

type P2PRemoveRelayServersResponse struct{}

// PolicyServerRefusal is returned when a policy server refuses to sign an
// event (MSC4284). ErrCode and Message come from the policy server's error.
type PolicyServerRefusal struct {
	StatusCode int
	ErrCode    string
	Message    string
}

func (e *PolicyServerRefusal) Error() string {
	return fmt.Sprintf("policy server refused the event (%d %s): %s", e.StatusCode, e.ErrCode, e.Message)
}
