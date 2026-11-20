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
