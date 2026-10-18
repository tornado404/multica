-- Add ZCode (`zcode`) to the built-in runtime profile protocol whitelist.
-- zcode (zcode-cli, https://github.com/kingsword09/zcode-cli) is a terminal
-- client for the ZCode Desktop agent runtime; multica drives it headlessly via
-- the native app-server session protocol. Kept in lockstep with
-- agent.SupportedTypes and agent.New(). NOT VALID preserves the historical-row
-- tolerance used by the prior family additions; the whitelist carries every
-- family added through migration 583 (muse) plus zcode. Renumbered
-- 491 -> 500 -> 583 -> 585: upstream v0.5.0 occupied 491, v0.6.x occupied
-- 500-582 (500_task_message_call_id collides by number), and v0.6.4 occupied
-- 583-584 (583_runtime_profile_add_muse collides by number); the runner
-- records by filename stem, so this re-applies harmlessly.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'codearts',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'reasonix',
        'dsh',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'mcode',
        'muse',
        'dim',
        'zeroclaw',
        'zcode'
    )) NOT VALID;
