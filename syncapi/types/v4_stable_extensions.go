// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package types

import (
	"encoding/json"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
)

// Extension keys of MSC4508, MSC4538, MSC4543, MSC4548 and MSC4551.
const (
	ExtensionTyping      = "typing"
	ExtensionToDevice    = "to_device"
	ExtensionE2EE        = "e2ee"
	ExtensionAccountData = "account_data"
	ExtensionReceipts    = "receipts"
)

// extensionPrefixes are the unstable prefixes these MSCs give their keys on
// /v4/sync until they are accepted.
var extensionPrefixes = map[string]string{
	ExtensionTyping:      "org.matrix.msc4508.",
	ExtensionToDevice:    "org.matrix.msc4538.",
	ExtensionE2EE:        "org.matrix.msc4543.",
	ExtensionAccountData: "org.matrix.msc4548.",
	ExtensionReceipts:    "org.matrix.msc4551.",
}

// ExtensionFormat is how an ExtensionResponse is written.
type ExtensionFormat struct {
	// Stable writes the formats MSC4508, MSC4538 and MSC4551 define, which
	// /v4/sync serves. Without it, the response keeps the formats of
	// Synapse's org.matrix.simplified_msc3575 endpoint, which existing
	// clients parse.
	Stable bool
	// Prefixed holds the extensions a /v4/sync request named with their
	// unstable prefix, whose responses carry the same key.
	Prefixed map[string]bool
}

// NormalizeStableExtensions prepares the extensions of a /v4/sync request:
// an extension named only with its unstable prefix is moved to its
// unprefixed field and remembered, so that its response uses the same key,
// and "*", which MSC4508 does not define, is dropped from the lists and rooms
// of the per-room extensions, so that it no longer matches everything.
func (r *ExtensionRequest) NormalizeStableExtensions() ExtensionFormat {
	format := ExtensionFormat{Stable: true, Prefixed: map[string]bool{}}
	if r == nil {
		return format
	}
	if r.Typing == nil && r.TypingMSC4508 != nil {
		r.Typing = r.TypingMSC4508
		format.Prefixed[ExtensionTyping] = true
	}
	if r.ToDevice == nil && r.ToDeviceMSC4538 != nil {
		r.ToDevice = r.ToDeviceMSC4538
		format.Prefixed[ExtensionToDevice] = true
	}
	if r.E2EE == nil && r.E2EEMSC4543 != nil {
		r.E2EE = r.E2EEMSC4543
		format.Prefixed[ExtensionE2EE] = true
	}
	if r.AccountData == nil && r.AccountDataMSC4548 != nil {
		r.AccountData = r.AccountDataMSC4548
		format.Prefixed[ExtensionAccountData] = true
	}
	if r.Receipts == nil && r.ReceiptsMSC4551 != nil {
		r.Receipts = r.ReceiptsMSC4551
		format.Prefixed[ExtensionReceipts] = true
	}

	if r.Typing != nil {
		r.Typing.Lists, r.Typing.Rooms = literalWildcards(r.Typing.Lists, r.Typing.Rooms)
	}
	if r.AccountData != nil {
		r.AccountData.Lists, r.AccountData.Rooms = literalWildcards(r.AccountData.Lists, r.AccountData.Rooms)
	}
	if r.Receipts != nil {
		r.Receipts.Lists, r.Receipts.Rooms = literalWildcards(r.Receipts.Lists, r.Receipts.Rooms)
	}
	return format
}

// DropUnstableExtensionKeys forgets the prefixed extension keys, which the
// org.matrix.simplified_msc3575 endpoint does not read.
func (r *ExtensionRequest) DropUnstableExtensionKeys() {
	if r == nil {
		return
	}
	r.TypingMSC4508, r.ToDeviceMSC4538, r.E2EEMSC4543, r.AccountDataMSC4548, r.ReceiptsMSC4551 = nil, nil, nil, nil, nil
}

// literalWildcards drops "*" from the lists and rooms of an extension, which
// /v4/sync does not treat as a wildcard (MSC4508): an array left with no
// other entry matches nothing, as MSC4508 defines for an empty array. No room
// ID is "*", and a list named "*" cannot be told apart from Synapse's
// wildcard, so it is not matched either.
func literalWildcards(lists, rooms []string) ([]string, []string) {
	isWildcard := func(entry string) bool { return entry == "*" }
	if lists != nil {
		lists = slices.DeleteFunc(slices.Clone(lists), isWildcard)
	}
	if rooms != nil {
		rooms = slices.DeleteFunc(slices.Clone(rooms), isWildcard)
	}
	return lists, rooms
}

// stableToDevice is the to_device section of MSC4538, which names the
// messages "messages" where Synapse's endpoint says "events".
type stableToDevice struct {
	NextBatch string                                `json:"next_batch"`
	Messages  []gomatrixserverlib.SendToDeviceEvent `json:"messages"`
}

// stableRooms is the typing section of MSC4508 and the receipts section of
// MSC4551: the content of the m.typing or m.receipt event of each room,
// without the event around it.
type stableRooms struct {
	Rooms map[string]spec.RawJSON `json:"rooms"`
}

// MarshalJSON writes the extensions in Format.
func (r ExtensionResponse) MarshalJSON() ([]byte, error) {
	type plain ExtensionResponse
	if !r.Format.Stable {
		return json.Marshal(plain(r))
	}

	// Write the sections this function rewrites separately, so that the rest
	// keep their encoding.
	rest := plain(r)
	rest.ToDevice, rest.Typing, rest.Receipts = nil, nil, nil
	data, err := json.Marshal(rest)
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	set := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		out[key] = encoded
		return nil
	}
	if r.ToDevice != nil {
		messages := r.ToDevice.Events
		if messages == nil {
			messages = []gomatrixserverlib.SendToDeviceEvent{}
		}
		if err = set(ExtensionToDevice, stableToDevice{NextBatch: r.ToDevice.NextBatch, Messages: messages}); err != nil {
			return nil, err
		}
	}
	if r.Typing != nil {
		if err = set(ExtensionTyping, stableRooms{Rooms: eventContents(r.Typing.Rooms)}); err != nil {
			return nil, err
		}
	}
	if r.Receipts != nil {
		if err = set(ExtensionReceipts, stableRooms{Rooms: eventContents(r.Receipts.Rooms)}); err != nil {
			return nil, err
		}
	}
	for key := range r.Format.Prefixed {
		if value, ok := out[key]; ok {
			delete(out, key)
			out[extensionPrefixes[key]+key] = value
		}
	}
	return json.Marshal(out)
}

// eventContents unwraps the event of each room.
func eventContents(rooms map[string]synctypes.ClientEvent) map[string]spec.RawJSON {
	contents := make(map[string]spec.RawJSON, len(rooms))
	for roomID, ev := range rooms {
		contents[roomID] = ev.Content
	}
	return contents
}
