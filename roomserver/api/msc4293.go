// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

// RedactEventsKey is the membership content key with which a kick or ban
// asks for the target's events to be redacted (MSC4293), with its unstable
// prefix.
const RedactEventsKey = "org.matrix.msc4293.redact_events"
