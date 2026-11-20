# Python 测试 → Go 测试对照（验收标准 B1）

每个 Python 测试都要有对应的 Go 测试，或者写明不迁的原因。模块迁完一个就补一段。

## services/config.py（无独立测试文件，行为用向量覆盖）

| Python | Go |
| --- | --- |
| `ConfigStore.get()` / `update()` 全部归一化与校验 | `internal/settings` `TestMatchesPythonConfigStore`（656 组向量，`tools/vectors/settings_vectors.py` 生成） |
| auth-key 来自环境变量或 config.json | `TestAuthKeyFromEnvOrFile` |

## test_account_image_capabilities.py

| Python | Go |
| --- | --- |
| test_image_accounts_require_positive_quota | `internal/pool` `TestImageAccountsNeedPositiveQuota` |
| test_prolite_variants_are_normalized | `TestAccountTypeAliases` |
| test_search_account_type_ignores_unrelated_scalar_values | 不迁：`_search_account_type` 除了递归调用自己，没有任何调用方（死代码） |
| test_mark_image_result_consumes_quota | `TestMarkImageResultConsumesQuota` |
| test_split_image_model_* | 第 4 步（生图链路） |
| test_get_available_access_token_filters_by_plan_type | `TestAvailableTokenFiltersByPlanType` |
| test_abnormal_account_keeps_the_error_text | `TestAbnormalAccountKeepsErrorText` |
| test_refresh_accounts_can_remove_invalid_token_without_confirmation_delay | `TestRefreshCanRemoveInvalidTokenWithoutDelay` |
| test_refresh_accounts_defers_invalid_token_removal_by_default | `TestRefreshDefersInvalidRemovalByDefault` |
| TokenLogTests.test_anonymize_token_hides_raw_value | `TestAnonymizeTokenHidesValue` |
| AuthServiceTests.test_create_authenticate_disable_and_delete_user_key | `internal/compat` `TestUserKeyNamesAndConflicts`、`TestLoginAndRoles` |
| AuthServiceTests.test_authenticate_ignores_last_used_save_failure | 不迁：Go 版 last_used_at 写库失败不影响鉴权（`Authenticate` 不检查写入结果） |
| AuthServiceTests.test_update_user_key_replaces_raw_key | `TestUserKeyNamesAndConflicts`（自定义 key 后旧 key 失效、新 key 可用） |
| AuthServiceTests.test_user_key_name_must_be_unique | `TestUserKeyNamesAndConflicts` |

## test_account_export.py

| Python | Go |
| --- | --- |
| test_build_export_items_uses_codex_shape_and_jwt_claims | `TestExportUsesCodexShapeAndJWTClaims` |
| test_build_export_items_skips_accounts_missing_complete_tokens | `TestExportSkipsIncompleteAccounts` |
| test_add_account_items_preserves_export_fields_without_overwriting_plan_type | `TestAddItemsKeepsExportFieldsWithoutTakingPlanType` |

## test_edit_upload_cooldown.py

| Python | Go |
| --- | --- |
| test_parse_hours_minutes_default_and_cap | `TestParseEditUploadCooldown` |
| test_detects_upload_limit_phrase | `TestParseEditUploadCooldown` |
| test_edits_skip_cooled_account_generations_do_not | `TestEditsSkipCooledAccountGenerationsDoNot` |
| test_mark_cooldown_and_refresh_keeps_field | `TestMarkCooldownSurvivesRefresh` |
| test_excluded_tokens_are_not_reused | `TestEditsSkipCooledAccountGenerationsDoNot`（排除的号不再被选中） |

## Go 版新增（Python 没有对应测试）

`TestRoundRobinAndConcurrencyLimit`（轮询、并发上限、等待被唤醒、并发位全部释放）、`TestNoQuotaMessages`、
`TestSidelinedAccountIsSkipped`、`TestReloadKeepsOrderAndFields`、`TestOnlyChangedAccountsAreWritten`、
`TestTokenRotationMovesSlotsAndAlias`、`TestStatsCountsStatuses`、`internal/compat` 的鉴权与设置接口测试。

## test_proxy_list.py

| Python | Go |
| --- | --- |
| ProxyListTests.test_mask_hides_only_the_password | `internal/proxylist` `TestMaskHidesOnlyThePassword` |
| test_list_seeds_account_proxies_and_keeps_names | `TestListSeedsAccountProxiesAndKeepsNames` |
| test_rename_add_and_delete_rules | `TestRenameAddAndDeleteRules` |
| test_consecutive_failures_mark_abnormal_and_success_recovers | `TestFailuresMarkAbnormalAndSuccessRecovers` |
| test_reserve_fills_least_loaded_counting_existing_accounts | `TestReserveFillsLeastLoadedCountingExistingAccounts` |
| test_reserve_skips_abnormal_unless_all_are_abnormal | `TestReserveSkipsAbnormalUnlessAllAre` |
| test_reserve_requires_a_candidate | `TestReserveRequiresACandidate` |
| test_reservations_count_until_released | `TestReservationsCountUntilReleased` |
| test_spellings_of_one_proxy_share_entry_health_and_load | `TestSpellingsOfOneProxyShareEntryHealthAndLoad` |
| ProxyHealthHookTests（4 个，Cloudflare 挑战与代理健康） | 第 4 步：上游请求层 |
| ImportDistributionTests（13 个，导入/协议登录的平均分配与预留） | 导入部分：`internal/compat` `TestImportRespectsThePerProxyCapAndSkipsDisabled`、`TestEnableBringsAProxyBackIntoDistribution`；协议登录部分：第 10 步 |
| ProxyBatchAddTests.test_colon_format_is_saved_as_url_and_the_password_is_hidden | `internal/compat` `TestBatchColonFormatIsStoredAsURLAndPasswordHidden` |
| test_numbering_continues_from_the_vendors_highest_number | `TestBatchNumberingContinuesFromVendorsHighest` |
| test_duplicates_and_invalid_lines_are_reported_and_do_not_use_numbers | `TestBatchDuplicatesAndInvalidLines` |
| test_without_vendor_names_stay_empty | `TestBatchWithoutVendorKeepsNamesEmpty` |
| test_empty_and_oversized_batches_are_rejected | `TestBatchRejectsEmptyAndOversized` |
| test_colon_format_proxy_on_an_account_is_masked_in_the_list | `TestColonProxyOnAnAccountIsMaskedInTheList` |

## test_proxy_reassign.py（全部走 HTTP 接口，在 `internal/compat/proxies_test.go`）

| Python | Go |
| --- | --- |
| RetireTests.test_dry_run_plans_least_loaded_moves_and_changes_nothing | `TestRetireDryRunPlansAndChangesNothing` |
| test_apply_moves_accounts_then_disables_and_logs | `TestRetireApplyMovesThenDisablesAndLogs` |
| test_capacity_shortfall_previews_and_blocks_apply | `TestRetireCapacityShortfallPreviewsAndBlocksApply` |
| test_abnormal_and_disabled_proxies_never_receive_accounts | `TestAbnormalAndDisabledProxiesNeverReceiveAccounts` |
| test_no_usable_target_is_an_error | `TestNoUsableTargetIsAnError` |
| test_retiring_an_empty_proxy_just_disables_it | `TestRetiringAnEmptyProxyJustDisablesIt` |
| test_enable_brings_a_proxy_back_into_distribution | `TestEnableBringsAProxyBackIntoDistribution` |
| DistributionCapTests.test_import_respects_the_per_proxy_cap_and_skips_disabled | `TestImportRespectsThePerProxyCapAndSkipsDisabled`（另查预留全部释放） |
| test_pick_least_loaded_is_all_or_nothing | `internal/proxylist` `TestPickLeastLoadedIsAllOrNothing` |
| ReassignTests.test_reassign_selected_accounts_to_all_usable_proxies | `TestReassignSelectedAccountsToAllUsableProxies` |
| test_reassign_to_chosen_proxies_only | `TestReassignToChosenProxiesOnly` |
| test_unknown_accounts_are_an_error | `TestReassignUnknownAccountsIsAnError` |
| CheckTests.test_one_failed_round_is_offered_for_retirement | `TestOneFailedRoundIsOfferedForRetirement` |
| test_disabled_proxies_are_not_tested | `TestDisabledProxiesAreNotTested` |
| test_widespread_failure_is_a_provider_outage_without_a_plan | `TestWidespreadFailureIsAProviderOutageWithoutAPlan` |
| test_all_402_failures_are_a_provider_outage | `internal/proxyops` `TestAll402FailuresAreAProviderOutage` |
| MaxAccountsConfigTests.test_defaults_and_bounds | `TestMaxAccountsSettingBounds` |

## test_image_stream_retry.py（`internal/imagegen/generate_test.go`）

| Python | Go |
| --- | --- |
| ImageModelNotFoundTests.test_only_exact_image_conversation_error_matches | `TestOnlyExactImageConversationErrorMatches` |
| test_account_cooldown_expires_without_changing_quota_or_status | `internal/pool` `TestSidelinedAccountIsSkipped`（暂避只在内存、不改账号） |
| test_disguised_404_moves_to_next_account | `TestDisguised404MovesToNextAccount` |
| test_other_404_is_not_retried | `TestOther404IsNotRetried` |
| test_failover_is_bounded_by_existing_account_retry_limit | `TestModelNotFoundFailoverIsBounded` |
| ImageForbiddenTests.test_403_cools_the_account_and_skips_its_egress_on_retry | `TestForbiddenCoolsTheAccountAndSkipsItsEgress` |
| test_403_falls_back_to_the_same_egress_when_nothing_else_is_free | `TestForbiddenFallsBackToTheSameEgressWhenNothingElseIsFree` |
| test_403_retries_are_bounded | `TestForbiddenRetriesAreBounded` |
| test_same_proxy_tokens_only_groups_accounts_with_that_proxy | `internal/pool` `TestSameProxyTokensOnlyGroupsThatProxy` |
| ImageStreamResetTests（5 个） | `TestDetectsOnlyHTTP2StreamResets`、`TestResetSwitchesToAnotherAccount`、`TestResetRetriesAtMostTwice`、`TestBreakerStopsRetryingDuringAWideOutage`、`TestOldResetsFallOutOfTheBreakerWindow` |
| ImageFailoverTests（5 个） | `TestFailoverSwitchesAccountWhenOneIsStuckAfterProgress`、`TestFailoverOffKeepsTheSingleLongWait`、`TestFailoverTotalWaitIsCappedByThePollTimeout`、`TestFailoverSwitchesAtMostFourTimes`、`TestExplicitPollTimeoutDisablesFailover` |
| ImageSlotReleaseTests（3 个） | `TestFailoverSwitchReleasesTheStuckAccountsSlot`、`TestFailoverExhaustedReleasesEverySlot`、`TestPlainTimeoutAndSuccessReleaseEverySlot` |
| ImageFailoverConfigTests.test_defaults_and_bounds | `TestFailoverSettingDefaultsAndBounds` |

## test_image_upscale.py（`internal/imagesize`）

Lanczos 与精确尺寸部分全部对应：`TestUpscaleKeepsRatio`、`TestUpscaleSkipsLargeAndExcessive`、`TestUpscaleKeepsAlpha`、
`TestRequestedLongEdge`、`TestRequestedDimensions`、`TestFitToSize`、`TestCropKeepsTheCenter`、`TestMaybeUpscalePipeline`。
RealESRGAN 模型相关的 5 个（model_output_size、tiling、failure fallback、slow model、unknown method）不迁：Go 版没有 ONNX 推理，
model 方式直接走 Lanczos（等同 Python 的失败回退）。

## test_image_fit.py（`internal/imagefit`）

`TestUnderBudgetReturnsOriginals`、`TestOverBudgetFitsThePackage`、`TestLongEdgeIsCapped`、`TestUnreadableImagesAreLeftAlone`、
`TestLeftoverBudget`、`TestExifOrientationIsApplied`。Python 用例中依赖 WebP 编码的断言不迁（Go 无 WebP 编码器）。

## test_image_output_counts.py（`internal/logs`）

| Python | Go |
| --- | --- |
| test_counts_successful_images_in_beijing_windows | `TestCountsSuccessfulImagesInBeijingWindows` |
| test_new_success_updates_the_daily_file | 同上（Go 版计数在 `Add` 里同一事务累加，没有单独的文件） |
| test_list_reads_the_newest_lines | `TestListReturnsNewestFirstWithFilters`（另加 `TestDeleteByID`） |
