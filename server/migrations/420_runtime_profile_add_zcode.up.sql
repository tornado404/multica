-- Add ZCode (`zcode`) to the built-in runtime profile protocol whitelist.
-- zcode (zcode-cli, https://github.com/kingsword09/zcode-cli) is a terminal
-- client for the ZCode Desktop agent runtime; multica drives it headlessly via
-- the native app-server session protocol. Kept in lockstep with
-- agent.SupportedTypes and agent.New(). NOT VALID preserves the historical-row
-- tolerance used by the prior family additions; the whitelist carries every
-- family added through migration 403 (zeroclaw) plus zcode.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
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
        'dim',
        'zeroclaw',
        'zcode'
    )) NOT VALID;
