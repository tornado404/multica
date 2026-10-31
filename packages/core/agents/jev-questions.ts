/**
 * The `jev_questions` entry of a jev agent's runtime_config: the agent-authored
 * question spec forwarded verbatim as the `questions` object of a TypeSafe
 * System One request. Structural validation lives on the server
 * (server/pkg/agent.ValidateJevQuestionsSpec) — this module only extracts and
 * re-serializes the value, plus a JSON syntax check for instant form feedback;
 * the server stays the boundary for shape rules.
 */
export const JEV_QUESTIONS_KEY = "jev_questions";

/**
 * Extract the question spec from a runtime_config, or undefined when the agent
 * has none yet. Returned as-is (unvalidated) — rendering it back into the
 * editor must not silently reshape what the author wrote.
 */
export function parseJevQuestionsSpec(
  runtimeConfig: Record<string, unknown> | null | undefined,
): unknown {
  const raw = runtimeConfig?.[JEV_QUESTIONS_KEY];
  return raw === undefined ? undefined : raw;
}

/**
 * Merge a question spec into the agent's existing runtime_config, preserving
 * every other key (gateway tokens for other runtimes must survive a save).
 */
export function withJevQuestionsSpec(
  runtimeConfig: Record<string, unknown> | null | undefined,
  questions: unknown,
): Record<string, unknown> {
  return { ...(runtimeConfig ?? {}), [JEV_QUESTIONS_KEY]: questions };
}

/** Parse the editor's text. `ok: false` means invalid JSON syntax. */
export function parseJevQuestionsJson(
  text: string,
): { ok: true; value: unknown } | { ok: false } {
  try {
    return { ok: true, value: JSON.parse(text) as unknown };
  } catch {
    return { ok: false };
  }
}

/**
 * The runtime_config key naming the LLM agent that image-involving jev tasks
 * escalate to (server: handler.validateJevEscalationTarget). Empty or absent
 * means unconfigured — the server then cancels image tasks with a visible
 * note instead of escalating.
 */
export const JEV_ESCALATION_AGENT_KEY = "jev_escalation_agent_id";

/** Empty string when unconfigured. */
export function parseJevEscalationAgentId(
  runtimeConfig: Record<string, unknown> | null | undefined,
): string {
  const raw = runtimeConfig?.[JEV_ESCALATION_AGENT_KEY];
  return typeof raw === "string" ? raw : "";
}

/** Merge an escalation target ("" to unset) into the existing runtime_config. */
export function withJevEscalationAgentId(
  runtimeConfig: Record<string, unknown> | null | undefined,
  agentId: string,
): Record<string, unknown> {
  return { ...(runtimeConfig ?? {}), [JEV_ESCALATION_AGENT_KEY]: agentId };
}
