-- Remove Jev from the protocol_family whitelist. Existing rows referencing
-- 'jev' keep their value (the constraint is NOT VALID); new rows are rejected.
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
        'devin'
    )) NOT VALID;
