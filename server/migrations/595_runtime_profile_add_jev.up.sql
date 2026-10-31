-- Add Jev (TypeSafe System One judgment models) as a first-party protocol
-- family. Unlike the CLI families, a jev runtime calls an HTTP API and spawns
-- no process; the family membership still gates which backends a custom
-- runtime profile may declare. Expand the whitelist without scanning
-- historical rows; replacing both parts in one ALTER keeps the constraint
-- present atomically.
ALTER TABLE runtime_profile
    DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check,
    ADD CONSTRAINT runtime_profile_protocol_family_check
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
        'devin',
        'jev'
    )) NOT VALID;
