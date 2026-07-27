// Package rights enumerates the fine-grained rights presentr declares to the holistic
// rights standard. Each constant is the Linux group backing one permission in
// permissions/presentr.json — keep the two in sync. Enforcement uses auth.User.Can,
// i.e. the standard rule: isAdmin || group ∈ groups. A host without privleg has empty
// hp_* groups, so non-admins resolve to admin-only.
package rights

// GroupUse backs permissions/presentr.json → room:use. Members (and admins) may open the
// presentation-room tutor: read the document pool, see and edit the connection diagram,
// and ask the assistant. presentr gates the whole service on one symmetric right — a
// presentation room is used or it is not; finer granularity would not map to a real
// distinction (Rechte-Axiom: Feingranularität nur, wo fachlich zwingend geboten).
const GroupUse = "hp_presentr_use"
