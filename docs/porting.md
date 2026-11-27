# Prototype → Go porting map

The behavioral reference is the fetched `origin/main` of the Python prototype.
All test inputs, archives and request traces are synthetic. No deployment data is used.
Package names below are relative to `internal/`, except `pluginapi` (`pkg/pluginapi`) and the CLI.
Parameterized cases are table subtests. A replay row names the exact prototype test function as a subtest.

## Independent fixtures and replay

`tools/generate-golden.py` invokes the prototype’s `test_viewer_port.snapshot` helper and
`FolderStorage.save`. `TestGoldenRoundTrip` verifies every object and re-saves byte-identical
JSON/manifest hashes; it does not manufacture expected bytes with Go code.
`tools/generate-content-cases.py` executes all 42 prototype content-validation cases.
`tools/generate-adapter-cases.py` executes 55 adapter cases and records 26 snapshot calls,
including complete request/response traces, failed captures and cached-attachment inputs.
Go replays successful snapshots against the prototype bytes. The prototype’s monkeypatched
2-byte budget test is replaced by a real 512 MiB declared-size preflight test.

## Case mapping

| Prototype module / test | Go coverage or scope decision |
|---|---|
| `test_storage.py::test_folder_roundtrip_and_unchanged_save_preserves_manifest` | storage/TestGoldenRoundTrip |
| `test_storage.py::test_folder_refuses_hash_mismatch_and_keeps_previous_snapshot` | storage/TestTamperAndInventory; core/TestCaptureFailureRetainsOld |
| `test_storage.py::test_folder_refuses_unmanifested_files_and_symlinks` | storage/TestTamperAndInventory |
| `test_storage.py::test_manifest_paths_cannot_escape_storage` | storage/TestTamperAndInventory/escape |
| `test_storage.py::test_reaction_user_order_is_a_noop` | storage/TestReactionMembershipNoop |
| `test_storage.py::test_git_commits_changes_and_unchanged_save_is_noop` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_never_stages_unrelated_content` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_push_to_bare_remote` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_ephemeral_rich_text_block_id_is_a_noop` | storage/TestEphemeralNoop |
| `test_storage.py::test_interrupted_snapshot_replacement_recovers_previous_generation` | storage/TestRecovery; TestCrashRecoveryProcess |
| `test_storage.py::test_git_unchanged_save_retries_unpushed_commit` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_push_rejects_a_different_push_remote` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_push_verifies_committed_objects_after_git_filters` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_recovers_a_failed_commit_without_accepting_unrelated_files` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_refuses_existing_non_main_branch` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_refuses_unrelated_untracked_files_in_snapshot_prefix` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_corrupt_worktree_snapshot_does_not_block_unrelated_thread` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_noop_skips_acknowledged_push_and_uses_configurable_timeout` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_accepts_finite_positive_float_timeout` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage.py::test_git_refuses_invalid_timeout` | Deferred to #8: git backend is explicitly outside M1. |
| `test_storage_catalog.py::test_tolerant_catalog_skips_one_corrupt_snapshot_and_preserves_bytes` | storage/TestCatalogIsolationAndFailures |
| `test_storage_catalog.py::test_catalog_streams_attachment_hashes_and_keeps_memory_bounded` | storage/TestCatalogStreamsLargeAttachment |
| `test_storage_catalog.py::test_renderer_hardlinks_verified_assets_and_keeps_old_release_valid` | Static publication removed by approved §6.4; verified on-request rendering is covered by web/TestViewerSecurity and TestViewerRejectsCorrupt. |
| `test_storage_catalog.py::test_identical_asset_publication_is_noop_but_rechecks_source_integrity` | Static publication removed by approved §6.4; verified on-request rendering is covered by web/TestViewerSecurity and TestViewerRejectsCorrupt. |
| `test_storage_catalog.py::test_publication_streams_copy_when_hardlink_crosses_filesystems` | Static publication removed by approved §6.4; verified on-request rendering is covered by web/TestViewerSecurity and TestViewerRejectsCorrupt. |
| `test_storage_catalog.py::test_changed_asset_after_catalog_verification_cannot_publish` | storage/TestLazyBlobRechecksIntegrity; web/TestViewerRejectsCorrupt |
| `test_storage_catalog.py::test_concurrent_site_publications_serialize_complete_releases` | Static publication removed by approved §6.4; verified on-request rendering is covered by web/TestViewerSecurity and TestViewerRejectsCorrupt. |
| `test_storage_catalog.py::test_git_lazy_attachment_comparison_still_reports_noop` | Deferred to #8: git backend. |
| `test_storage_catalog.py::test_tolerant_catalog_does_not_drop_channel_for_stray_file` | storage/TestCatalogIsolationAndFailures |
| `test_storage_catalog.py::test_keys_lists_roots_without_reading_or_hashing_attachment_bytes` | storage/TestKeysDoesNotReadObjects |
| `test_storage_catalog.py::test_keys_returns_corrupt_snapshot_identity_and_reports_stray_paths` | storage/TestCatalogIsolationAndFailures |
| `test_storage_catalog.py::test_keys_recovers_an_interrupted_snapshot_rename` | storage/TestRecovery; TestCrashRecoveryProcess |
| `test_slack_web.py::test_complete_snapshot_hashes_and_read_only_calls` | slack/TestReferenceAdapterCases/test_complete_snapshot_hashes_and_read_only_calls |
| `test_slack_web.py::test_identity_mismatch_stops_channel_reads` | slack/TestIdentityDiscoveryAndHistoryBound |
| `test_slack_web.py::test_discovery_filters_archived_and_foreign_channels` | slack/TestIdentityDiscoveryAndHistoryBound |
| `test_slack_web.py::test_replies_repeated_root_deduplicated` | slack/TestReferenceAdapterCases/test_replies_repeated_root_deduplicated |
| `test_slack_web.py::test_invalid_history_pagination_fails_closed` | slack/TestPagination |
| `test_slack_web.py::test_repeated_reply_not_deduplicated` | slack/TestReferenceAdapterCases/test_repeated_reply_not_deduplicated |
| `test_slack_web.py::test_incomplete_or_unsupported_thread_fails_closed` | slack/TestReferenceAdapterCases/test_incomplete_or_unsupported_thread_fails_closed |
| `test_slack_web.py::test_reply_cannot_be_trigger_root` | slack/TestReactionRootBoundary |
| `test_slack_web.py::test_mentions_are_stored_but_unavailable_mentions_optional` | slack/TestReferenceAdapterCases/test_mentions_are_stored_but_unavailable_mentions_optional |
| `test_slack_web.py::test_client_refuses_every_write_before_transport` | slack/TestReadOnlyAndSafeErrors |
| `test_slack_web.py::test_attachment_url_rejects_unsafe_components` | slack/TestURLSafety; TestReferenceAdapterCases |
| `test_slack_web.py::test_permalink_preserves_valid_context` | slack/TestURLSafety; storage/TestGoldenRoundTrip |
| `test_slack_web.py::test_permalink_rejects_unsafe_context` | slack/TestURLSafety |
| `test_slack_web.py::test_redirect_validated_before_credentials_are_forwarded` | slack/TestFileRedirectValidated |
| `test_slack_web.py::test_api_error_does_not_expose_response_or_token` | slack/TestReadOnlyAndSafeErrors |
| `test_slack_web.py::test_truncated_http_body_is_sanitized` | slack/TestHTTPBoundsRedirectAndRetry/truncated |
| `test_slack_web.py::test_snapshot_orders_timestamps_numerically` | slack/TestReferenceAdapterCases/test_snapshot_orders_timestamps_numerically |
| `test_slack_web.py::test_snapshot_attachment_budget_checked_before_download` | slack/TestSnapshotBudgetBeforeDownload (real 512 MiB declared-size cap) |
| `test_slack_web.py::test_attachment_url_rejects_non_string_metadata` | slack/TestReferenceAdapterCases |
| `test_slack_web.py::test_snapshot_keeps_raw_preview_and_unknown_content_without_external_fetch` | slack/TestReferenceAdapterCases/test_snapshot_keeps_raw_preview_and_unknown_content_without_external_fetch |
| `test_slack_web.py::test_unchanged_attachment_reuses_verified_previous_bytes` | slack/TestReferenceAdapterCases/test_unchanged_attachment_reuses_verified_previous_bytes |
| `test_slack_web.py::test_changed_attachment_metadata_downloads_new_bytes` | slack/TestReferenceAdapterCases/test_changed_attachment_metadata_downloads_new_bytes |
| `test_slack_web.py::test_corrupt_cached_attachment_never_reused` | slack/TestReferenceAdapterCases/test_corrupt_cached_attachment_never_reused |
| `test_slack_web.py::test_incremental_history_passes_oldest_to_slack` | slack/TestIdentityDiscoveryAndHistoryBound |
| `test_slack_web.py::test_verified_lazy_attachment_reused_without_network` | slack/TestReferenceAdapterCases/test_verified_lazy_attachment_reused_without_network |
| `test_slack_web.py::test_deleted_file_never_satisfied_from_cached_bytes` | slack/TestReferenceAdapterCases/test_deleted_file_never_satisfied_from_cached_bytes |
| `test_socket.py::test_app_connection_uses_separate_post_credential` | slack/TestEndToEnd/socket; fake AssertCredentials |
| `test_socket.py::test_app_connection_validates_socket_url` | slack/TestSocketURL |
| `test_socket.py::test_socket_ack_filters_workspace_and_queues_event` | slack/TestSocketFiltersAndAckFullQueue |
| `test_socket.py::test_socket_acknowledges_full_queue_without_blocking` | slack/TestSocketFiltersAndAckFullQueue |
| `test_triggers.py::test_required_reactions_and_root_only` | core/TestTrigger |
| `test_triggers.py::test_allowed_owner_must_supply_all_reactions` | core/TestTrigger |
| `test_triggers.py::test_incomplete_owner_membership_fails_closed` | core/TestTrigger |
| `test_triggers.py::test_any_accepts_full_counts_even_if_slack_omits_users` | core/TestTrigger |
| `test_events.py::test_reactions_target_only_message_roots` | slack/TestEventNormalization (source normalization lives in plugin) |
| `test_events.py::test_edits_and_deletes_map_back_to_thread_root` | slack/TestEventNormalization (source normalization lives in plugin) |
| `test_events.py::test_malformed_event_is_ignored` | slack/TestEventNormalization (source normalization lives in plugin) |
| `test_runtime.py::test_unsupported_capability_skips_and_logs` | core/TestUnsupportedHistoryAndUnmarkedEvents (capability behavior; diagnostic wording intentionally unpinned) |
| `test_runtime.py::test_startup_and_periodic_backfill_share_queue` | core/TestWorkspaceIsolation; TestQueueBoundAndCoalescing |
| `test_runtime.py::test_unmarked_message_does_not_fetch_thread` | core/TestUnsupportedHistoryAndUnmarkedEvents |
| `test_runtime.py::test_consumer_survives_unexpected_platform_error` | core/TestCaptureFailureRetainsOld; TestWorkspaceIsolation/panic (Go panics restart a workspace per design) |
| `test_runtime.py::test_queue_coalesces_backfill_and_duplicate_events` | core/TestQueueBoundAndCoalescing; TestBackfillQueueCoalescing |
| `test_runtime.py::test_thread_failure_does_not_mark_entire_workspace_unavailable` | core/TestCaptureFailureRetainsOld; TestWorkspaceIsolation |
| `test_runtime.py::test_discord_startup_scan_begins_after_ready_without_collection_gap` | Deferred to #6: other-platform startup barrier. |
| `test_backfill.py::test_successful_capture_logs_identity_once_without_message_or_credentials` | core/TestCaptureLogsIdentityOnlyOnce |
| `test_backfill.py::test_backfill_filters_unmarked_roots_and_uses_incremental_history` | core/TestHistorySkipsUnmarkedCandidates; TestBackfillRepairsAndIncremental |
| `test_backfill.py::test_failed_capture_is_reported_with_safe_reason` | core/TestCaptureFailureRetainsOld; TestWorkspaceIsolation; pluginapi/TestRedactingLogger |
| `test_backfill.py::test_corrupt_snapshot_does_not_republish_on_every_backfill` | storage/TestCatalogIsolationAndFailures; per-request viewer replaces publication |
| `test_backfill.py::test_history_failure_still_refreshes_existing_archive` | core/TestBackfillRepairsAndIncremental |
| `test_content_validation.py::test_rich_message_mention_preserves_validated_link_and_escapes_label` | slack/TestReferenceContentCases/test_rich_message_mention_preserves_validated_link_and_escapes_label |
| `test_content_validation.py::test_unsafe_message_mention_is_plain_text` | slack/TestReferenceContentCases/test_unsafe_message_mention_is_plain_text |
| `test_content_validation.py::test_malformed_message_mentions_fail_closed` | slack/TestReferenceContentCases/test_malformed_message_mentions_fail_closed |
| `test_content_validation.py::test_canvas_reference_is_text_and_never_downloaded` | slack/TestReferenceContentCases/test_canvas_reference_is_text_and_never_downloaded |
| `test_content_validation.py::test_malformed_canvas_rejected` | slack/TestReferenceContentCases/test_malformed_canvas_rejected |
| `test_content_validation.py::test_mentions_include_secondary_and_rich_user_elements` | slack/TestReferenceContentCases/test_mentions_include_secondary_and_rich_user_elements |
| `test_content_validation.py::test_unsupported_or_oversized_content_is_rejected` | slack/TestReferenceContentCases/test_unsupported_or_oversized_content_is_rejected |
| `test_content_validation.py::test_nested_attachment_files_and_text_are_preserved` | slack/TestReferenceContentCases/test_nested_attachment_files_and_text_are_preserved |
| `test_content_validation.py::test_common_rich_and_unknown_elements_have_safe_plain_text_fallback` | slack/TestReferenceContentCases/test_common_rich_and_unknown_elements_have_safe_plain_text_fallback |
| `test_content_validation.py::test_image_link_preview_preserves_labels_without_fetching_media` | slack/TestReferenceContentCases/test_image_link_preview_preserves_labels_without_fetching_media |
| `test_content_validation.py::test_non_string_block_type_is_archive_error` | slack/TestReferenceContentCases/test_non_string_block_type_is_archive_error |
| `test_content_validation.py::test_file_block_without_downloadable_metadata_still_fails_closed` | slack/TestReferenceContentCases/test_file_block_without_downloadable_metadata_still_fails_closed |
| `test_content_validation.py::test_future_widget_style_metadata_does_not_block_text_capture` | slack/TestReferenceContentCases/test_future_widget_style_metadata_does_not_block_text_capture |
| `test_content_validation.py::test_cosmetic_style_metadata_preserves_text_without_rendering_metadata` | slack/TestReferenceContentCases/test_cosmetic_style_metadata_preserves_text_without_rendering_metadata |
| `test_content_validation.py::test_style_metadata_remains_subject_to_content_bounds` | slack/TestReferenceContentCases/test_style_metadata_remains_subject_to_content_bounds |
| `test_render_mentions.py::test_display_name_precedence_matches_slack` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_malformed_non_author_entries_never_break_rendering` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_author_and_mention_use_the_same_precedence` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_unknown_mention_falls_back_to_names_from_other_archived_threads` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_a_threads_own_users_win_over_the_global_map` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_global_map_conflicts_resolve_deterministically_to_the_newest_snapshot` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_render_mentions.py::test_rich_text_user_element_renders_as_a_named_mention` | slack/TestNamesAndMentions; web/TestGlobalMentionPrecedence |
| `test_config.py::test_environment_secrets_and_defaults` | config/TestLoad |
| `test_config.py::test_bot_token_alone_validates` | config/TestLoad |
| `test_config.py::test_workspaces_can_mix_polling_and_realtime` | config/TestMixedTransportAndCleanupSecretIsolation |
| `test_config.py::test_supplied_app_token_is_validated` | config/TestAppTokenValidation |
| `test_config.py::test_invalid_config_rejected` | config/TestInvalidConfig |
| `test_config.py::test_duplicate_workspace_storage_rejected` | config/TestOverlap |
| `test_config.py::test_check_config_without_secrets_for_offline_render` | config/TestOffline; cmd/chatkeeper/TestOfflineCLI |
| `test_config.py::test_nested_workspace_storage_rejected` | config/TestOverlap |
| `test_config.py::test_git_push_timeout_is_configurable_and_bounded` | Deferred to #8: git configuration; M1 rejects non-folder storage. |
| `test_config.py::test_loaded_git_config_constructs_storage_with_default_timeout` | Deferred to #8: git configuration; M1 rejects non-folder storage. |
| `test_config.py::test_api_token_env_is_optional_redacted_and_configurable` | config/TestBearerConfig; pluginapi/TestCredentialsRedacted |
| `test_config.py::test_protocol_routes_are_reserved_workspace_names` | config/TestReservedWorkspaceNames |
| `test_config.py::test_offline_config_can_ignore_unused_invalid_api_token` | config/TestBearerConfig |
| `test_config.py::test_cleanup_config_rejects_unsafe_options` | Deferred to #5: cleanup authorization/configuration. M1 rejects enabled cleanup and destructive switches (config/TestInvalidConfig). |
| `test_config.py::test_archive_config_does_not_load_cleanup_secret` | config/TestMixedTransportAndCleanupSecretIsolation |
| `test_config.py::test_cleanup_age_and_authorization` | Deferred to #5: cleanup authorization/configuration. M1 rejects enabled cleanup and destructive switches (config/TestInvalidConfig). |
| `test_config.py::test_telegram_cleanup_requires_owner` | Deferred to #6/#7: other-platform configuration. |
| `test_config.py::test_telegram_cleanup_rejects_impossible_age` | Deferred to #6/#7: other-platform configuration. |
| `test_config.py::test_discord_cleanup_explicit_own_only_without_collector_secret` | Deferred to #6/#7: other-platform configuration. |
| `test_config.py::test_cleanup_own_only_requires_boolean` | Deferred to #5: cleanup authorization/configuration. M1 rejects enabled cleanup and destructive switches (config/TestInvalidConfig). |

## New acceptance and safety coverage

- `architecture/TestPlatformBoundaries`; `plugins/all/TestExcludedRegistryEmpty` under `noslack`; the excluded binary prints `plugins: 0`.
- `slack/TestEndToEnd/socket` and `/polling`: source reaction → complete archive → verified viewer, with separate app/bot credentials.
- `storage/TestCrashRecoveryProcess`: actual subprocess kill at observed stage/rename publication boundaries; `TestRecovery` independently pins rollback behavior.
- `storage/TestCleanupPin`: pending, partial, completed and malformed cleanup journals.
- `core/TestWorkspaceIsolation`: Start panic, hung history/content/Stop and flood while another workspace captures.
- `core/TestStopTimeoutQuarantinesFailedStart` and `TestStartHangQuarantinesOnFailure`: no repeated creation while Stop or Start remains alive.
- `pluginapi/TestAttachmentReferenceSerializable`: inline/file-backed attachments remain plain contract data.
- `slack/TestPluginContract`: manifest/schema/setup subset, capability ceiling, cancellation, Stop bound, unsupported cleanup, goroutine leak check.
- `web/TestViewerSecurity`: bearer auth, methods, no-script/sandbox CSP, escaping and forced attachment downloads.
- CI: vet, staticcheck, race suite, noslack suite/binary, gitleaks, six static cross-builds.

## M1 implementation decisions

- The Go floor is 1.26 as designed; mise’s additional 1.26.11 installation supplies confined rename operations. CI uses the latest 1.26 patch.
- The initial YAML-only viewer uses plain escaped text and resolved mention labels. Pixel parity with the prototype static-site markup is not a requirement; static export remains out of scope.
- Setup-flow types ship in v1, but this built-in returns no wizard steps until M2. No htmx, database, admin session, config writer or cleanup engine is pulled into M1.
- Attachment descriptors are serializable inline bytes or verified file references. The latter preserve bounded-memory reads and can be streamed by a future RPC adapter.
- On Windows, file fsync and rename provide process-crash recovery; directory fsync is unavailable. Native Windows/macOS runtime tests and live-workspace verification remain release/cutover work.

## Never-drop review corrections

The issue #1 invariant supersedes the prototype's destructive refresh behavior.
Schema-1 archives remain byte-identical on an unchanged round trip. Additive
schema-2 history is written only when merge introduces annotations; stop older
writers before upgrading shared storage. See architecture §6.2 for the exact
version/hash, deletion, reappearance, and explicit removal contracts.

- `storage/TestRefreshNeverDrops`, `TestEditReappearAndExplicitRemoval`,
  `TestSavingArchivedDeletionIsNoop`, and `TestRejectTamperedHistory`: source
  deletion retains content/files, prior hashes verify, markers are stable,
  archival round trips preserve absence, reappearance is recorded, explicit
  removal is the deletion boundary.
- `core/TestMissingRootRetainsArchive`, `TestArchivedReappearanceWithoutTrigger`,
  `TestFailedFullBackfillRetriesFull`, and
  `TestOverflowLogsAndRepairsDroppedKey`: missing-root persistence, failed full
  scan retry, logged overflow with repair tracking. Isolation flood keys are
  distinct; backfill has a reserved slot.
- `slack/TestNeverDropLifecycle` and `TestSocketDeletionRefreshNeverDrops`: real HTTP adapter → core → disk exercises reply
  deletion, root deletion, edit history hashes, reappearance, and explicit removal.
- `storage/TestAdditionalGoldenRoundTrips`: independent synthetic no-files and
  multi-reply schema-1 fixtures, canonical Python JSON bytes and manifest hashes.
- `config/TestPublicBindRequiresToken`, `web/TestBindAndRemovalStub`, and
  `TestMentionCacheAvoidsRepeatedArchiveScans`: public-bind startup refusal,
  authenticated disabled removal API, bounded-frequency mention scans.
- `slack/TestSlackFileRedirectTargets`: realistic private-file and signed
  `slack-files.com` redirect shapes, credential stripping, and lookalike/scheme
  rejection using synthetic URLs. No live workspace or signed URLs are used.
  The initial authenticated URL remains restricted to `files.slack.com`; signed
  redirects allow only the exact `slack-files.com` host, with no bearer or cookies.
  This follows [Slack's credential and redirect guidance](https://docs.slack.dev/messaging/working-with-files/#downloading-files).

## M3 search, REST and MCP

The reference files are `search.py`, `server/api.py`, `server/mcp.py` and their
search/API/MCP/server tests at the prototype's `origin/main`. Paths below are
package-relative unless prefixed with `cmd/`.

| Prototype case | Go coverage / decision |
|---|---|
| `test_index_bootstraps_from_disk_and_survives_restart` | search/TestSearchBootstrapFiltersAndUnicode; TestRebuildMissingCorruptAndStale |
| `test_filters_and_full_verified_json` | search/TestSearchBootstrapFiltersAndUnicode; web/TestRESTArchiveQueriesPaginationAndSearchHTML |
| `test_ranked_pagination_is_query_bound_and_invalidated_on_edit` | search/TestPaginationAndValidation; TestNeverDropIncrementalAndStaleCursor |
| `test_rebuild_removes_missing_and_corrupt_threads_without_touching_archive` | search/TestRebuildOmitsCorruptAndRemovedThreads |
| `test_bad_queries_are_sanitized` (all parameter cases) | search/TestPaginationAndValidation; TestQuerySyntaxErrors; TestTimeValidationAndPrecision; web/TestRESTAuthValidationAndOpenAPI |
| `test_capture_updates_index_on_edits_and_noop_retries` | web/TestCaptureSearchEditDeletionAndNoopRepair; retained old text is intentional schema-2 behavior |
| `test_author_filter_includes_reply_authors` | search/TestPythonGoldenParity; web/TestRESTArchiveQueriesPaginationAndSearchHTML; real SDK author query in cmd/chatkeeper/exerciseMCP |
| `test_missing_thread_lookup_does_not_create_archive_directories` | search/TestRebuildOmitsCorruptAndRemovedThreads; storage/TestReaderDoesNotRecoverOrCreateFiles |
| `test_concurrent_rebuilds_leave_complete_index` | search/TestConcurrentCaptureRebuildAndQueries (rebuild/query/capture share serialization; archive lock extends through index commit) |
| `test_cursor_with_oversized_numeric_score_is_rejected` | search/TestPaginationAndValidation plus typed finite-score cursor decoding; malformed cursor cases in REST/MCP |
| `test_thread_reads_wait_for_git_archive_replacement` (both init orders) | storage/TestReaderWaitsForCapturePublication; TestReaderDoesNotRecoverOrCreateFiles. Folder publication gap is covered; actual git backend remains #8. |
| `test_first_page_search_tolerates_capture_before_query` | search/TestConcurrentCaptureRebuildAndQueries; read-only external-capture client in cmd/chatkeeper/TestStdioObservesExternalCapture |
| `test_index_query_revision_and_rows_share_read_snapshot` | Per-index SQL read transaction plus service lock; concurrent capture/query test and revision-bound cursor test |
| `test_incremental_sync_holds_archive_lock_through_index_write` | storage/Catalog and Inspect keep lock through callback; search/TestConcurrentCaptureRebuildAndQueries; TestSharedCaptureUpdatesOwnerAndRejectsWrongSource |
| `test_search_initialization_can_be_deferred_per_workspace` | Deliberately replaced by atomic startup/configuration readiness; rejected candidate never publishes. search/TestFailedConfigureRetainsSearchAndThread; TestPublicationFailureRestoresOldIndex |
| `test_database_io_failure_is_not_a_bad_query` | Safe ErrUnavailable separate from explicit FTS syntax classification; plugin failure and corrupt-thread 503 tests. No driver mock injected. |
| `test_api_search_thread_and_openapi_on_same_viewer_server` | web/TestRESTArchiveQueriesPaginationAndSearchHTML; TestRESTAuthValidationAndOpenAPI |
| `test_bad_api_search_is_json_400` (all cases) | web/TestRESTAuthValidationAndOpenAPI; TestRESTArchiveQueriesPaginationAndSearchHTML |
| `test_optional_auth_protects_all_content_routes` | web/TestRESTAuthValidationAndOpenAPI; existing viewer auth tests. HTTP MCP always requires bearer; REST additionally allows admin session. |
| `test_corrupt_thread_and_index_errors_are_sanitized` | search/TestRebuildOmitsCorruptAndRemovedThreads; web API error mapping and cmd/chatkeeper MCP error calls |
| `test_bearer_scheme_is_case_insensitive` | web/TestRESTAuthValidationAndOpenAPI |
| `test_mcp_streamable_http_protocol_roundtrip_with_auth` | cmd/chatkeeper/TestMCPHTTPClient; TestMCPGoldenHTTPTranscript; all three read-only tools |
| `test_mcp_stdio_is_offline_and_stdout_is_protocol_only` | cmd/chatkeeper/TestMCPStdioClient; TestMCPGoldenStdioTranscript; TestStdioDoesNotCreateStateOrChangeArchive |
| `test_mcp_thread_io_errors_do_not_expose_paths` / `test_mcp_search_io_errors_do_not_expose_paths` | Shared service sanitizes archive/plugin errors; real client exercises missing thread/invalid query, secret/path checks; search/TestPluginFailuresAreBoundedAndSanitized |
| `test_viewer_routes_health_downloads_and_blocks_escape` / `test_directory_index_cannot_follow_symlink_outside_viewer` / `test_workspace_routes_cannot_cross_archives` | Existing web/TestViewerSecurity and storage confinement tests; no static filesystem serving added for API routes |
| `test_health_reports_workspace_failure_without_restarting_viewer` | Existing workspace-isolation/health tests; web/TestSlowReloadDoesNotBlockAppRequests; health remains public liveness |

Additional coverage includes missing FTS tables/rows/origin metadata, SQLite file
corruption, extraction identity changes, failed reloads and file-publication
rollback, opaque root IDs with explicit timestamps, selected-workspace isolation,
plugin panic/deadline containment, token environment overrides/legacy fallback,
proxy Origin validation, owner/follower backfill, and Chrome search submission
from both dashboard and sandboxed viewer in desktop/server modes.

### Golden parity and protocol transcripts

`internal/search/testdata/corpus.json` is shared input. `scripts/search-parity.py`
invokes the Python reference's actual `SearchService`, verified `FolderStorage`
and snapshot helper in a temporary directory. Regenerate using that checkout's
Python environment:

```sh
python scripts/search-parity.py --reference <reference-checkout>
```

The committed `python-results.json` contains independently generated result order,
BM25 scores and snippets for thirteen plain-text/Unicode/phrase/prefix/boolean/
filter queries. `TestPythonGoldenParity` compares those against real Go archives
and FTS5. Viewer links intentionally use the Go `/view/` route. Binary attachment
contents and platform-specific rendering are outside this corpus and retain their
separate M1 port tests. Schema-2 historical search intentionally extends Python's
current-only refresh behavior.

`internal/mcp/testdata/transcript.json` records initialize, initialized notification,
tools/list and calls to all three tools plus invalid query/missing thread. The CLI
replays it over actual subprocess stdio and streamable HTTP; expected subsets omit
transport-dependent IDs, optional capability fields and dynamic scores. Stdio may
interleave the SDK's standard tools/list_changed notification with a response.
Independent official SDK clients exercise both transports as well.

### Baseline and deliberate implementation decisions

Measured on Linux/amd64, Intel Xeon W-2155, Go 1.26, pure-Go SQLite, synthetic
10,000 single-message threads with ASCII, accented and CJK text, no attachments:

| Operation | Baseline | Command |
|---|---:|---|
| Full verified rebuild | 6.29 s (one iteration) | `go test ./internal/search -run '^$' -bench BenchmarkRebuild10K -benchtime=1x` |
| Persistent-index query | 28.04 ms mean / 41.30 ms p95 (100 queries) | `go test ./internal/search -run '^$' -bench BenchmarkQuery10K -benchtime=100x` |
| Original per-query archive rebuild (review baseline) | 3.35 s mean / 3.49 s p95 (5 queries, nearest rank) | `go test ./internal/search -run '^$' -bench BenchmarkReadOnlyQuery10K -benchtime=5x` |

These are baselines, not CI timing gates. CPU/cache/filesystem contention and
attachment verification affect results. The writable index uses WAL, one SQL
connection, embedded numbered schema and deterministic archive-derived revisions.
Only confirmed corruption/schema mismatch discards derived files; transient I/O
errors fail safely. Archive reads never recover or delete staged data themselves.

Stdio now opens the persistent index read-only, with read-only SHM for live WAL.
Cold immutable reads hold existing index writer/publication leases, excluding
future writers until the query closes. No archive lock is used. Fallback streams
verified snapshots without archive locks and has hard traversal/text/page/time
budgets; concurrent publication may temporarily omit a fallback result. Missing
stores fail closed. See README for limits and the CJK substring/ranking contract.

The review-fix benchmark on the same 10k corpus measured **3213 ms p95 before**
(20 queries) and **47.65 ms p95 after** (100 queries), using
`BenchmarkReadOnlyQuery10K`. The after measurement includes reopening the on-disk
index on each query. After the WAL/bounds review fixes, a fresh 100-query run measured **42.22 ms p95**
(30.26 ms mean).

Python golden score/snippet parity still applies to FTS queries within each
workspace. Cross-workspace ordering intentionally groups workspace IDs, and
literal CJK uses substring semantics; these reviewed deviations have dedicated
Go tests rather than inheriting the prototype's misses.

Opaque root IDs remain searchable; time filters require a parseable explicit
root `timestamp` or epoch root ID. HTTP MCP is stateless JSON, bearer-only, and
Origin-checked; an absent token does not make MCP public. REST's `cursor` alias
preserves prototype clients while `after` matches the design. No archive format
change is introduced by M3. Native Windows/macOS runtime testing remains release
work; CI keeps all existing jobs and six cgo-free cross-builds.
