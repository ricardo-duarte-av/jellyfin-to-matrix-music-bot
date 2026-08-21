package rtc

import (
	"context"

	"maunium.net/go/mautrix"
)

// The unstable feature flags a homeserver advertises in the unstable_features
// section of /_matrix/client/versions. MatrixRTC needs both: sticky events are
// the transport for membership, MatrixRTC is what gives them meaning.
var (
	// FeatureStickyEvents is MSC4354: sticky events.
	FeatureStickyEvents = mautrix.UnstableFeature{UnstableFlag: "org.matrix.msc4354"}
	// FeatureMatrixRTC is MSC4143: slots and m.rtc.member.
	FeatureMatrixRTC = mautrix.UnstableFeature{UnstableFlag: "org.matrix.msc4143"}
	// FeatureMatrixRTCStable is what a homeserver advertises once MSC4143 has
	// completed FCP and it implements the stable identifiers.
	FeatureMatrixRTCStable = mautrix.UnstableFeature{UnstableFlag: "org.matrix.msc4143.stable"}
)

// Capabilities is what the homeserver says it can do, as far as this bot cares.
type Capabilities struct {
	// StickyEvents reports MSC4354 support.
	StickyEvents bool
	// MatrixRTC reports MSC4143 support, stable or unstable.
	MatrixRTC bool
}

// SupportsStickyMembership reports whether the homeserver can carry the
// sticky-event membership stack. Both halves are needed: sticky m.rtc.member
// events without MSC4143 are events nobody interprets, and MSC4143 without
// stickiness has no way to express membership at all.
func (c Capabilities) SupportsStickyMembership() bool {
	return c.StickyEvents && c.MatrixRTC
}

// Probe asks the homeserver which unstable features it supports.
//
// A failed probe is not fatal: it returns zero capabilities, which reads as
// "legacy only" — the same conservative answer as a homeserver that genuinely
// supports nothing.
func Probe(ctx context.Context, client *mautrix.Client) (Capabilities, error) {
	versions, err := client.Versions(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		StickyEvents: versions.Supports(FeatureStickyEvents),
		MatrixRTC:    versions.Supports(FeatureMatrixRTC) || versions.Supports(FeatureMatrixRTCStable),
	}, nil
}
