package webhook

import "errors"

var (
	// ErrSelfHostedClusterDeletionBlocked is returned when a DELETE targets the
	// Cluster hosting the Kommodity management plane itself.
	ErrSelfHostedClusterDeletionBlocked = errors.New(
		"deletion of the self-hosted cluster is blocked: this cluster hosts the Kommodity management plane; " +
			"complete a reverse pivot (state handover) first or set the " +
			"kommodity.io/allow-self-hosted-delete=true annotation for a deliberate teardown")
	// ErrSelfHostedMarkerRemovalBlocked is returned when an UPDATE strips the
	// self-hosted marker annotation without the override annotation being set.
	ErrSelfHostedMarkerRemovalBlocked = errors.New(
		"removal of the kommodity.io/self-hosted marker is blocked: set the " +
			"kommodity.io/allow-self-hosted-delete=true annotation to confirm a deliberate teardown")
	// ErrUnexpectedObjectType is returned when an admission request carries an
	// object of an unexpected type.
	ErrUnexpectedObjectType = errors.New("unexpected object type in admission request")
)
