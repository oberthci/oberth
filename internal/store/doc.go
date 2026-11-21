// Package store persists identities, accepted work and evidence with atomic audit records.
// Audit reads verify page links and the preceding link. Backup, restore and
// explicit archive verification verify the whole local chain. Native execution records
// progress directly; the service supplies administrative facts and publication intents.
// Default branches are read from source delivery, never stored here.
// Spec: docs/plans/c7-durable-state.md#owned-data-and-public-contract
package store
