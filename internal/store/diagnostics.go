package store

import "github.com/oberthci/oberth/internal/publicerr"

func diagnostics() map[refusal]diagnostic {
	result := coreDiagnostics()
	for kind, value := range queryDiagnostics() {
		result[kind] = value
	}
	for _, part := range []map[refusal]diagnostic{workDiagnostics(), identityDiagnostics(), issueDiagnostics()} {
		for kind, message := range part {
			result[kind] = message
		}
	}
	return result
}

func coreDiagnostics() map[refusal]diagnostic {
	return map[refusal]diagnostic{
		scheduleAdmissionExpired: {publicerr.Forbidden, "scheduled admission is no longer retained", "Use a child of the current schedule or its immediately preceding admission."},
		invalid: {publicerr.InvalidInput, "state input is invalid",
			"Supply valid bounded identities and values."},
		missing: {publicerr.NotFound, "state record was not found",
			"Read the current records and select an existing identity."},
		conflict: {publicerr.Conflict, "state record conflicts with an existing fact",
			"Read the existing record and submit its current identity."},
		unavailable: {publicerr.Unavailable, "database access failed",
			"Check the database path and permissions, then retry."},
		schema: {publicerr.SchemaRefused, "This version requires a fresh database.",
			"Create a fresh database for this version; no upgrade path is supported."},
		foreignProtocol: {publicerr.SchemaRefused, "Database belongs to another installation.",
			"Use the installation this database belongs to."},
		newerSchema: {publicerr.SchemaRefused, "Database schema is newer than this executable.",
			"Use the executable that supports this database version."},
		forbidden: {publicerr.Forbidden, "authority is missing or expired",
			"Request an active grant or an explicit scope change."},
		corrupt: {publicerr.SchemaRefused, "database integrity verification failed",
			"Preserve the damaged database and select an independently verified recovery archive."},
		ambiguous: {publicerr.Ambiguous, "state selector matches more than one record",
			"Use the complete repository name or run identity."},
		transition: {publicerr.InvalidTransition, "requested state transition is invalid",
			"Read the current status and choose an allowed transition."},
		space: {publicerr.Unavailable, "snapshot space is insufficient",
			"Free space or move the database to a larger volume."},
		tooLarge: {publicerr.InvalidInput, "database exceeds the snapshot limit",
			"Raise the database byte limit within the 4 GiB maximum."},
		locked: {publicerr.Unavailable, "database writer is locked",
			"Wait for the active writer to finish, then retry within a new deadline."},
		diskFull: {publicerr.Unavailable, "database volume is full",
			"Free disk space or quota on the database and staging volumes, then retry."},
		damagedArchive: {publicerr.SchemaRefused, "recovery archive is damaged or inconsistent",
			"Select another authenticated archive and verify it before restoring."},
		stageBusy: {publicerr.Conflict, "snapshot staging is in use",
			"Wait for the other snapshot or restore operation to finish."},
		exists: {publicerr.Conflict, "destination already exists",
			"Select an absent destination path."},
	}
}

func workDiagnostics() map[refusal]diagnostic {
	return map[refusal]diagnostic{
		workflowBound: {publicerr.Conflict, "run already has a bound Workflow",
			"Observe its recorded Workflow instead of failing an unbound run."},
		unboundReason: {publicerr.InvalidInput, "unbound failure reason is not in the status catalogue",
			"Use a defined submission or recovery reason and put the full diagnostic in Detail."},
		attemptInFlight: {publicerr.Conflict, "publication attempt is already in flight",
			"Wait for the recorded attempt to settle before starting another."},
		retryDeferred: {publicerr.Conflict, "publication retry time has not arrived",
			"Wait until the recorded retry time or explicitly reset the retry budget."},
		noAdmission: {publicerr.InvalidTransition, "run has no admitted execution plan",
			"Inspect the recorded admission findings and submit a corrected run."},
		staleRevision: {publicerr.Conflict, "run revision changed",
			"Read the run again and submit the current revision."},
		terminalRun: {publicerr.Conflict, "run is already terminal",
			"Use its recorded result or request an eligible rerun."},
		pendingPublication: {publicerr.Conflict, "publication owns the run settlement",
			"Settle or abandon the pending publication before changing the run."},
		bindingChanged: {publicerr.Conflict, "native workflow binding differs from the admitted run",
			"Use the exact admitted repository, revision and workflow identity."},
		purgeProtected: {publicerr.Conflict, "run evidence is protected by durable work",
			"Complete its obligations and pending publication before purging."},
		purgeRunning: {publicerr.Conflict, "run is still active",
			"Wait for a terminal verdict before purging evidence."},
		purgeReceiptChanged: {publicerr.Conflict, "purge receipt differs from the recorded intent",
			"Use the receipt recorded when the purge began."},
		incompleteEvidence: {publicerr.Conflict, "native evidence sequence is incomplete",
			"Complete the current native observation sequence before using its evidence."},
		attemptChanged: {publicerr.Conflict, "publication attempt changed",
			"Read the publication and use its current attempt number."},
		publicationSettled: {publicerr.Conflict, "publication is already settled",
			"Use the recorded outcome; retry only a failed or deferred publication."},
		lockHeld: {publicerr.Conflict, "issue is locked by another actor",
			"Wait for that lock to expire or ask its owner to release it."},
		issueChanged: {publicerr.Conflict, "issue version changed",
			"Read the issue and submit its current version."},
		nodeBinding: {publicerr.Conflict, "native observation binding changed",
			"Use the exact admitted workflow, node and credential identities."},
		nodeRegression: {publicerr.InvalidTransition, "terminal native observation cannot regress",
			"Keep the recorded terminal observation or correct the source evidence."},
		nodeRetention: {publicerr.Conflict, "native observation lost recorded exit or timing evidence",
			"Preserve all previously recorded exit and timing evidence."},
		eventDedupe: {publicerr.Conflict, "event deduplication key has different content",
			"Reuse the key only for the identical event or choose a new key."},
	}
}

func identityDiagnostics() map[refusal]diagnostic {
	return map[refusal]diagnostic{
		repositoryBusy: {publicerr.Conflict, "repository has active runs or pending work",
			"List its runs and pending publications; wait for them to finish or cancel them before archiving."},
		assistNotRequested: {publicerr.Forbidden, "assistance was not requested for this run",
			"Start a run with assistance explicitly enabled."},
		assistedEvidence: {publicerr.Forbidden, "assisted work cannot provide green evidence",
			"Use a passed run that did not receive assistance."},
		repositoryBound: {publicerr.Conflict, "repository still owns durable history or storage",
			"Retain the repository until its history and storage ownership are resolved."},
		upstreamBound: {publicerr.Conflict, "upstream still owns repositories",
			"Remove its repositories before removing the upstream."},
		upstreamChanged: {publicerr.Conflict, "upstream binding is different or permanently retired",
			"Select a new upstream identity or use the recorded binding."},
		foreignOrganization: {publicerr.Conflict, "repository organization differs from its upstream",
			"Use the organization permanently bound to the selected upstream."},
		claimBound: {publicerr.Conflict, "volume claim is permanently owned by another repository or tier",
			"Select a different claim or use its recorded owner and tier."},
		invitationHeld: {publicerr.Conflict, "Invitation redemption is temporarily reserved.",
			"Wait one minute before retrying; if it still fails, ask the administrator for a new invitation."},
		invitationExpired: {publicerr.Forbidden, "invitation expired or its requested scope differs",
			"Obtain a fresh invitation with the exact intended identity and scope."},
		lastAdmin: {publicerr.Conflict, "operation would remove the last usable administrator",
			"Enroll another administrator with an active credential first."},
	}
}

func issueDiagnostics() map[refusal]diagnostic {
	return map[refusal]diagnostic{
		receiveChanged: {publicerr.Conflict, "receive identity was replayed with different content",
			"Reuse the exact recorded request or submit a new receive identity."},
	}
}

func queryDiagnostics() map[refusal]diagnostic {
	return map[refusal]diagnostic{
		invalidTrigger:  {publicerr.InvalidInput, "run trigger is invalid", "Select branch, tag, promotion or schedule."},
		invalidGlob:     {publicerr.InvalidInput, "reference glob is invalid", "Supply a valid reference glob without backslash escaping."},
		invalidCommit:   {publicerr.InvalidInput, "commit selector is not a hexadecimal prefix", "Supply a commit or object hexadecimal prefix of at most 64 characters."},
		ambiguousCommit: {publicerr.Ambiguous, "commit selector matches multiple visible commits", "Supply a longer visible object or commit prefix."},
	}
}
