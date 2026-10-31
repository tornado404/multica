// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  JEV_QUESTIONS_KEY,
  parseJevEscalationAgentId,
  parseJevQuestionsJson,
  parseJevQuestionsSpec,
  withJevEscalationAgentId,
  withJevQuestionsSpec,
} from "./jev-questions";

describe("parseJevQuestionsSpec", () => {
  it("returns undefined when the agent has no spec", () => {
    expect(parseJevQuestionsSpec(undefined)).toBeUndefined();
    expect(parseJevQuestionsSpec({})).toBeUndefined();
    expect(parseJevQuestionsSpec(null)).toBeUndefined();
  });

  it("returns the spec value as stored, including null", () => {
    const spec = { tone: { type: "choice" } };
    expect(parseJevQuestionsSpec({ [JEV_QUESTIONS_KEY]: spec })).toBe(spec);
    expect(parseJevQuestionsSpec({ [JEV_QUESTIONS_KEY]: null })).toBeNull();
  });
});

describe("withJevQuestionsSpec", () => {
  it("merges the spec while preserving other runtime_config keys", () => {
    const existing = { gateway: { token: "t" }, jev_questions: { old: true } };
    const next = withJevQuestionsSpec(existing, { new: true });
    expect(next.gateway).toEqual({ token: "t" });
    expect(next.jev_questions).toEqual({ new: true });
  });

  it("creates a runtime_config when none exists", () => {
    expect(withJevQuestionsSpec(undefined, { a: 1 })).toEqual({
      jev_questions: { a: 1 },
    });
  });
});

describe("parseJevQuestionsJson", () => {
  it("parses valid JSON", () => {
    expect(parseJevQuestionsJson('{"a": 1}')).toEqual({
      ok: true,
      value: { a: 1 },
    });
  });

  it("rejects invalid JSON", () => {
    expect(parseJevQuestionsJson("{not json")).toEqual({ ok: false });
    expect(parseJevQuestionsJson("")).toEqual({ ok: false });
  });
});

describe("parseJevEscalationAgentId", () => {
  it("returns empty string when unconfigured", () => {
    expect(parseJevEscalationAgentId(undefined)).toBe("");
    expect(parseJevEscalationAgentId({})).toBe("");
    expect(parseJevEscalationAgentId({ jev_escalation_agent_id: null })).toBe(
      "",
    );
  });

  it("returns the configured id", () => {
    expect(
      parseJevEscalationAgentId({ jev_escalation_agent_id: "abc-123" }),
    ).toBe("abc-123");
  });
});

describe("withJevEscalationAgentId", () => {
  it("sets the target while preserving other keys", () => {
    const next = withJevEscalationAgentId(
      { jev_questions: { a: 1 }, other: true },
      "agent-2",
    );
    expect(next.jev_escalation_agent_id).toBe("agent-2");
    expect(next.jev_questions).toEqual({ a: 1 });
    expect(next.other).toBe(true);
  });

  it("unsets with an empty string", () => {
    expect(
      withJevEscalationAgentId({ jev_escalation_agent_id: "x" }, "")
        .jev_escalation_agent_id,
    ).toBe("");
  });
});
