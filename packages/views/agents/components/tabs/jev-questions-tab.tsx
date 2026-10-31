"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Loader2, Save } from "@multica/ui/icons";
import type { Agent } from "@multica/core/types";
import {
  parseJevEscalationAgentId,
  parseJevQuestionsJson,
  parseJevQuestionsSpec,
  withJevEscalationAgentId,
  withJevQuestionsSpec,
} from "@multica/core/agents";
import { agentListOptions } from "@multica/core/workspace";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { toast } from "sonner";
import { useT } from "../../../i18n";

// The editor holds the spec as text; dirty and save both work on the parsed
// value so reformatting whitespace alone is not a change worth saving.
function specToText(spec: unknown): string {
  if (spec === undefined || spec === null) return "";
  try {
    return `${JSON.stringify(spec, null, 2)}\n`;
  } catch {
    return "";
  }
}

export function JevQuestionsTab({
  agent,
  onSave,
  onDirtyChange,
}: {
  agent: Agent;
  onSave: (updates: { runtime_config: Record<string, unknown> }) => Promise<void>;
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const { t } = useT("agents");

  const original = useMemo(
    () => specToText(parseJevQuestionsSpec(agent.runtime_config)),
    [agent.runtime_config],
  );
  const originalEscalation = useMemo(
    () => parseJevEscalationAgentId(agent.runtime_config),
    [agent.runtime_config],
  );

  const [text, setText] = useState(original);
  const [escalation, setEscalation] = useState(originalEscalation);
  const [saving, setSaving] = useState(false);

  // Sync local draft when the agent prop changes — same pattern as
  // RuntimeConfigTab. Only adopt the new server value when the user has no
  // in-flight edits relative to the previous original.
  const previousOriginalRef = useRef({ original, originalEscalation });
  useEffect(() => {
    setText((current) =>
      current === previousOriginalRef.current.original ? original : current,
    );
    setEscalation((current) =>
      current === previousOriginalRef.current.originalEscalation
        ? originalEscalation
        : current,
    );
    previousOriginalRef.current = { original, originalEscalation };
  }, [original, originalEscalation]);

  const agentsQuery = useQuery(agentListOptions(agent.workspace_id));
  // The escalation target must be a different, active agent; the server
  // enforces the non-jev-runtime rule at save time.
  const escalationCandidates = useMemo(
    () =>
      (agentsQuery.data ?? []).filter(
        (candidate) =>
          candidate.id !== agent.id && !candidate.archived_at,
      ),
    [agentsQuery.data, agent.id],
  );
  const escalationMissing =
    !!escalation &&
    agentsQuery.isSuccess &&
    !escalationCandidates.some((candidate) => candidate.id === escalation);

  const parsed = useMemo(() => parseJevQuestionsJson(text), [text]);
  const dirty =
    (text !== original || escalation !== originalEscalation) && parsed.ok;
  // An empty editor means "no spec yet"; anything else must at least parse.
  const canSave = !saving && (text.trim() === "" || parsed.ok);

  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const handleSave = async () => {
    if (!canSave) return;
    setSaving(true);
    try {
      const value = text.trim() === "" ? null : parsed.ok ? parsed.value : null;
      await onSave({
        runtime_config: withJevEscalationAgentId(
          withJevQuestionsSpec(agent.runtime_config, value),
          escalation,
        ),
      });
      toast.success(t(($) => $.tab_body.jev_questions.saved_toast));
    } catch (err) {
      toast.error(
        err instanceof Error && err.message
          ? err.message
          : t(($) => $.tab_body.jev_questions.save_failed_toast),
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex h-full flex-col space-y-4">
      <fieldset className="space-y-2">
        <Label htmlFor="jev-questions" className="text-caption font-medium">
          {t(($) => $.tab_body.jev_questions.questions_label)}
        </Label>
        <Textarea
          id="jev-questions"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={t(($) => $.tab_body.jev_questions.placeholder)}
          spellCheck={false}
          aria-invalid={!parsed.ok || undefined}
          className="min-h-64 font-mono text-caption"
        />
        {!parsed.ok && (
          <p className="text-caption text-destructive">
            {t(($) => $.tab_body.jev_questions.invalid_json)}
          </p>
        )}
        <p className="text-caption text-muted-foreground">
          {t(($) => $.tab_body.jev_questions.state_hint)}
        </p>
      </fieldset>

      <fieldset className="space-y-2">
        <Label
          htmlFor="jev-escalation-agent"
          className="text-caption font-medium"
        >
          {t(($) => $.tab_body.jev_questions.escalation_label)}
        </Label>
        <div className="flex items-center gap-2">
          <Select
            items={escalationCandidates.map((candidate) => ({
              value: candidate.id,
              label: candidate.name,
            }))}
            value={escalation || null}
            onValueChange={(next) => setEscalation(next ?? "")}
          >
            <SelectTrigger
              id="jev-escalation-agent"
              className="w-full"
              aria-invalid={escalationMissing || undefined}
            >
              <SelectValue
                placeholder={t(
                  ($) => $.tab_body.jev_questions.escalation_unset,
                )}
              />
            </SelectTrigger>
            <SelectContent alignItemWithTrigger={false}>
              {escalationCandidates.map((candidate) => (
                <SelectItem key={candidate.id} value={candidate.id}>
                  {candidate.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {escalation && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setEscalation("")}
            >
              {t(($) => $.tab_body.jev_questions.escalation_clear)}
            </Button>
          )}
        </div>
        <p className="text-caption text-muted-foreground">
          {t(($) => $.tab_body.jev_questions.escalation_hint)}
        </p>
        {escalationMissing && (
          <p role="alert" className="text-caption text-destructive">
            {t(($) => $.tab_body.jev_questions.escalation_missing)}
          </p>
        )}
      </fieldset>

      <div className="flex items-center justify-end gap-3 pt-2">
        {dirty && (
          <span className="text-caption text-muted-foreground">
            {t(($) => $.tab_body.common.unsaved_changes)}
          </span>
        )}
        <Button onClick={handleSave} disabled={!dirty || !canSave} size="sm">
          {saving ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <Save className="h-3.5 w-3.5" />
          )}
          {t(($) => $.tab_body.common.save)}
        </Button>
      </div>
    </div>
  );
}
