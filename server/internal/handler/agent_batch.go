package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Shared agent-config patch seam (Allocator)
//
// The Allocator GUI re-buckets agents by (runtime, model) in bulk: dragging a
// batch of agents from one container onto another changes `runtime_id`,
// `model`, and (when the target provider family differs) `thinking_level` /
// `service_tier` for every agent in the selection.
//
// Those four fields cannot be re-derived per entry point. `UpdateAgent`
// resolves the *target* runtime and then decides, among other things, that a
// cross-provider move must clear a `model` the new runtime will never
// recognise, and that a `thinking_level` / `service_tier` the new provider
// cannot express is a 400 rather than a silent coercion. A batch endpoint
// that reimplemented any part of that arbitration would eventually disagree
// with the single-agent dialog — and the disagreement would be invisible until
// an agent started failing on the daemon. So `planAgentRuntimeSwap` is the one
// place that logic lives, and both `UpdateAgent` and `BatchUpdateAgents` call
// it.
//
// Errors are returned as `agentPatchError`, which carries the HTTP status the
// single-agent path writes and the machine-readable reason the batch path
// reports, so the two surfaces cannot drift on *why* a patch was refused.
// ---------------------------------------------------------------------------

// Machine-readable refusal reasons for `BatchUpdateAgentsResponse.Skipped`.
// Kept in one place because the client branches on them to render copy.
const (
	patchReasonForbidden          = "forbidden"
	patchReasonNotFound           = "not_found"
	patchReasonArchived           = "archived"
	patchReasonRuntimeNotFound    = "runtime_not_found"
	patchReasonRuntimePrivate     = "runtime_private"
	patchReasonRuntimeRequired    = "runtime_id_required"
	patchReasonThinkingInvalid    = "thinking_invalid"
	patchReasonServiceTierInvalid = "service_tier_invalid"
	patchReasonInternal           = "internal_error"
)

// agentPatchError is a refused agent config patch. `status` is what the
// single-agent endpoint writes to the wire; `reason` is what the batch endpoint
// reports per row.
type agentPatchError struct {
	reason string
	status int
	msg    string
}

func (e *agentPatchError) Error() string { return e.msg }

func patchErrorf(status int, reason, format string, args ...any) *agentPatchError {
	return &agentPatchError{reason: reason, status: status, msg: fmt.Sprintf(format, args...)}
}

// patchError carries an already-rendered message. The `*Rejection` helpers in
// agent.go return finished prose built from provider-supplied strings, which
// may contain a literal '%', so they must never be used as a format string.
func patchError(status int, reason, msg string) *agentPatchError {
	return &agentPatchError{reason: reason, status: status, msg: msg}
}

// agentRuntimeSwap carries the two "explicit clear" decisions made while
// planning a runtime/model swap. `UpdateAgent`'s SQL is COALESCE-based, so it
// cannot write NULL into thinking_level / service_tier — the caller applies
// those clears through the dedicated queries via `apply`.
type agentRuntimeSwap struct {
	clearThinkingLevel bool
	clearServiceTier   bool
}

// apply runs the planned clear queries against `q` (which may be a
// transaction-scoped Queries) and returns the row in its final state.
//
// The wrapped error text is deliberately just the column name so callers
// compose the same message they always have: "failed to clear thinking_level:
// <db error>".
func (s agentRuntimeSwap) apply(ctx context.Context, q *db.Queries, updated db.Agent) (db.Agent, error) {
	if s.clearThinkingLevel {
		cleared, err := q.ClearAgentThinkingLevel(ctx, updated.ID)
		if err != nil {
			return db.Agent{}, fmt.Errorf("thinking_level: %w", err)
		}
		updated = cleared
	}
	if s.clearServiceTier {
		cleared, err := q.ClearAgentServiceTier(ctx, updated.ID)
		if err != nil {
			return db.Agent{}, fmt.Errorf("service_tier: %w", err)
		}
		updated = cleared
	}
	return updated, nil
}

// canManageAgentFor is the pure half of the agent-management rule: a workspace
// owner/admin, or the human who owns the agent. It is the same predicate
// `canManageAgentEnv` documents and mirrors, so `canManageAgentEnv` delegates
// to it rather than keeping a second copy of the rule.
//
// The owner comparison runs against `member.UserID`, not a raw request header:
// `agent.owner_id` is nullable (migration 001) and `uuidToString` renders a
// NULL UUID as "", so comparing against a header that can also be empty would
// make every NULL-owner agent manageable by anyone. The empty-owner guard
// closes that case from the other side too.
func canManageAgentFor(agentRow db.Agent, member db.Member) bool {
	if roleAllowed(member.Role, "owner", "admin") {
		return true
	}
	ownerID := uuidToString(agentRow.OwnerID)
	return ownerID != "" && ownerID == uuidToString(member.UserID)
}

// planAgentRuntimeSwap resolves the runtime/model/thinking_level/service_tier
// slice of an agent update onto `params`, validating against the provider of
// the runtime that will be in force *after* the update.
//
// `member` is the caller's already-resolved workspace membership, needed for
// the private-runtime gate. Tri-state contract (same as UpdateAgentRequest):
// pointer nil = field omitted = leave alone; "" = explicit clear; non-empty =
// set.
func (h *Handler) planAgentRuntimeSwap(
	ctx context.Context,
	member db.Member,
	existing db.Agent,
	req UpdateAgentRequest,
	params *db.UpdateAgentParams,
) (agentRuntimeSwap, *agentPatchError) {
	var swap agentRuntimeSwap

	// Resolve the runtime that will be in force after this update so the
	// thinking_level validation hits the right provider enum. When the request
	// doesn't move the agent, we still need the *current* runtime to validate a
	// thinking_level change. Resolve once and reuse.
	targetRuntimeID := existing.RuntimeID
	targetProvider := ""
	if req.RuntimeID != nil {
		if *req.RuntimeID == "" {
			// UpdateAgent's SQL is `runtime_id = COALESCE($7, runtime_id)`, so a
			// NULL here would be silently discarded and the caller would believe
			// the agent was unbound. Un-binding is a different product action
			// (it goes through runtime teardown, which also drains tasks), so
			// refuse rather than no-op.
			return swap, patchError(http.StatusBadRequest, patchReasonRuntimeRequired,
				"runtime_id cannot be cleared through this endpoint; unbind the runtime from the runtime page")
		}
		runtimeUUID, err := util.ParseUUID(*req.RuntimeID)
		if err != nil {
			return swap, patchError(http.StatusBadRequest, patchReasonRuntimeNotFound, "invalid runtime_id")
		}
		runtime, err := h.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
			ID:          runtimeUUID,
			WorkspaceID: existing.WorkspaceID,
		})
		if err != nil {
			return swap, patchError(http.StatusBadRequest, patchReasonRuntimeNotFound, "invalid runtime_id")
		}
		// Same gate as CreateAgent — prevents an update (single or batch) from
		// being used to re-bind an agent onto someone else's private runtime,
		// which would otherwise be a quiet end-run around the CreateAgent check.
		if !canUseRuntimeForAgent(member, runtime) {
			return swap, patchError(http.StatusForbidden, patchReasonRuntimePrivate,
				"this runtime is private; only its owner can move agents onto it")
		}
		params.RuntimeID = runtime.ID
		params.RuntimeMode = pgtype.Text{String: runtime.RuntimeMode, Valid: true}
		targetRuntimeID = runtime.ID
		targetProvider = runtime.Provider
	}

	if req.Model != nil {
		params.Model = pgtype.Text{String: *req.Model, Valid: true}
	} else if req.RuntimeID != nil && existing.Model.Valid && agent.ModelKnownIncompatibleWithProvider(targetProvider, existing.Model.String) {
		// Model is runtime-native. When moving an agent across known provider
		// families and the caller did not choose a replacement model, clear the
		// old value so the new runtime falls back to its own default instead of
		// receiving an obvious foreign model ID (e.g. Claude Code -> Codex).
		// Unknown/custom model strings are preserved by the helper.
		params.Model = pgtype.Text{String: "", Valid: true}
	}

	// thinking_level handling (MUL-2339). Tri-state semantics:
	//   - field omitted  → leave column alone (COALESCE narg), but if a
	//     runtime change in this same request would make the *existing*
	//     value invalid for the new provider's fixed enum or token syntax,
	//     reject 400. Exact dynamic-catalog compatibility is daemon-owned.
	//   - field set to "" → explicit clear (run ClearAgentThinkingLevel post-update)
	//   - field set to value → validate against the target runtime's fixed enum
	//     or dynamic-token syntax; reject literal-invalid with 400. Per-model
	//     combination checks run in the daemon at execution time, not here.
	if req.ThinkingLevel != nil {
		value := *req.ThinkingLevel
		if value == "" {
			swap.clearThinkingLevel = true
		} else {
			// Need the target runtime's provider to validate. Re-fetch only when
			// we haven't already loaded it above (i.e. the request didn't change
			// runtime_id), to keep the no-change path one DB roundtrip.
			provider, perr := h.providerForSwap(ctx, existing, targetProvider, targetRuntimeID)
			if perr != nil {
				return swap, perr
			}
			if !agent.IsKnownThinkingValue(provider, value) {
				return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid, thinkingLevelRejection(provider, value))
			}
			switch h.acpThinkingDecision(ctx, provider, targetRuntimeID) {
			case acpEffortAbsent:
				return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid, thinkingCapabilityRejection(provider))
			case acpEffortUnknown:
				return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid, thinkingCapabilityUnknownRejection(provider))
			}
			params.ThinkingLevel = pgtype.Text{String: value, Valid: true}
		}
	} else if req.RuntimeID != nil && existing.ThinkingLevel.Valid && existing.ThinkingLevel.String != "" {
		// Runtime is changing but the caller didn't touch thinking_level. If the
		// existing value is not in the new provider's enum at all, preserving it
		// would smuggle a literal-invalid token to the daemon. Hold the same
		// line as the explicit-set path: always 400 on literal-invalid, never
		// silently coerce. The caller can either pass thinking_level="" to clear
		// or pick a value valid for the new runtime.
		provider, perr := h.providerForSwap(ctx, existing, targetProvider, targetRuntimeID)
		if perr != nil {
			return swap, perr
		}
		if !agent.IsKnownThinkingValue(provider, existing.ThinkingLevel.String) {
			return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid,
				existingThinkingLevelRejection(provider, existing.ThinkingLevel.String))
		}
		switch h.acpThinkingDecision(ctx, provider, targetRuntimeID) {
		case acpEffortAbsent:
			return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid,
				existingThinkingCapabilityRejection(provider, existing.ThinkingLevel.String))
		case acpEffortUnknown:
			return swap, patchError(http.StatusBadRequest, patchReasonThinkingInvalid,
				existingThinkingCapabilityUnknownRejection(provider, existing.ThinkingLevel.String))
		}
	}

	if req.ServiceTier != nil {
		value := *req.ServiceTier
		if value == "" {
			swap.clearServiceTier = true
		} else {
			provider, perr := h.providerForSwap(ctx, existing, targetProvider, targetRuntimeID)
			if perr != nil {
				return swap, perr
			}
			if !agent.IsKnownServiceTier(provider, value) {
				return swap, patchErrorf(http.StatusBadRequest, patchReasonServiceTierInvalid,
					"service_tier %q is not a recognised value for runtime %q", value, provider)
			}
			params.ServiceTier = pgtype.Text{String: value, Valid: true}
		}
	} else if req.RuntimeID != nil && existing.ServiceTier.Valid && existing.ServiceTier.String != "" {
		provider, perr := h.providerForSwap(ctx, existing, targetProvider, targetRuntimeID)
		if perr != nil {
			return swap, perr
		}
		if !agent.IsKnownServiceTier(provider, existing.ServiceTier.String) {
			return swap, patchErrorf(http.StatusBadRequest, patchReasonServiceTierInvalid,
				"existing service_tier %q is not valid for runtime %q; pass service_tier=\"\" to clear or set a value valid for the new runtime",
				existing.ServiceTier.String, provider)
		}
	}

	return swap, nil
}

// providerForSwap returns the provider of the runtime the agent will run on
// after the patch. When the request already moved the runtime, the resolved
// provider is handed in and no lookup is needed; otherwise resolve it from the
// agent's current runtime.
func (h *Handler) providerForSwap(
	ctx context.Context,
	existing db.Agent,
	resolved string,
	targetRuntimeID pgtype.UUID,
) (string, *agentPatchError) {
	if resolved != "" {
		return resolved, nil
	}
	provider, ok := h.resolveAgentProvider(ctx, existing.WorkspaceID, targetRuntimeID)
	if !ok {
		return "", patchError(http.StatusInternalServerError, patchReasonInternal,
			"failed to resolve runtime for thinking_level validation")
	}
	return provider, nil
}

// Stage sentinels for finalizeAgentMutationResponse. They exist so UpdateAgent
// can keep answering a failed read with the exact message it always used
// (shared with GetAgent / ListAgents) while the *shape* of the response is
// built in one place.
var (
	errAgentTargetsFetch = errors.New("failed to load agent invocation targets")
	errAgentSkillsFetch  = errors.New("failed to load agent skills")
)

// finalizeAgentMutationResponse builds the wire response for a just-updated
// agent and publishes the broadcast. Both update entry points call it so the
// payload they put on the socket is byte-identical: clients patch their Query
// cache from the broadcast, and a response that zeroes `skills` or omits
// invocation targets would silently wipe those projections (#3459).
//
// The returned response is already redacted for the actor.
func (h *Handler) finalizeAgentMutationResponse(
	ctx context.Context,
	r *http.Request,
	updated db.Agent,
	userID string,
) (AgentResponse, string, string, error) {
	resp := h.agentToResponse(updated)
	if err := h.enrichAgentResponseWithTargets(ctx, &resp, updated.ID); err != nil {
		return resp, "", "", fmt.Errorf("%w: %v", errAgentTargetsFetch, err)
	}
	// agentToResponse always initialises Skills as []; junction-table rows are
	// untouched by the SQL update, so reload them to keep the response (and the
	// broadcast that mirrors it) in sync with reality.
	if err := h.attachAgentSkills(ctx, &resp, updated.ID); err != nil {
		return resp, "", "", fmt.Errorf("%w: %v", errAgentSkillsFetch, err)
	}

	actorType, actorID := h.resolveActor(r, userID, uuidToString(updated.WorkspaceID))
	h.publish(protocol.EventAgentStatus, uuidToString(updated.WorkspaceID), actorType, actorID, map[string]any{
		"agent": broadcastAgentResponse(resp),
	})

	redactAgentResponseForActor(&resp, actorType)
	// Workspace admins / non-owner members pass the manage gate for legitimate
	// admin actions (e.g. bulk reassigning agents off a leaving member's
	// runtime), but they must not learn the agent owner's composio allowlist
	// from the mutation response.
	if !h.composioMCPAppsEnabled(ctx) {
		suppressComposioToolkitAllowlist(&resp)
	} else if uuidToString(updated.OwnerID) != userID {
		redactComposioToolkitAllowlist(&resp)
	}
	return resp, actorType, actorID, nil
}

// ---------------------------------------------------------------------------
// POST /api/agents/batch-update
// ---------------------------------------------------------------------------

// maxBatchUpdateAgents bounds one request's work. The Allocator's realistic
// ceiling is one agent per configured bot (tens), so 200 is generous headroom
// while keeping the worst case — 200 sequential row locks in one transaction —
// well inside a request timeout.
const maxBatchUpdateAgents = 200

// batchUpdateAllowedFields is the accepted key set per entry. Anything else is
// refused outright rather than dropped: a caller who sends `mcp_config` in a
// batch must learn it is unsupported here, not believe 20 agents had their MCP
// config rewritten. (`custom_env` in particular has its own audited endpoint —
// see the hard reject in UpdateAgent and MUL-2600.)
var batchUpdateAllowedFields = map[string]struct{}{
	"agent_id":       {},
	"runtime_id":     {},
	"model":          {},
	"thinking_level": {},
	"service_tier":   {},
}

type batchUpdateAgentEntry struct {
	AgentID       string  `json:"agent_id"`
	RuntimeID     *string `json:"runtime_id"`
	Model         *string `json:"model"`
	ThinkingLevel *string `json:"thinking_level"`
	ServiceTier   *string `json:"service_tier"`
}

// BatchUpdateAgentsRequest is the body of POST /api/agents/batch-update.
type BatchUpdateAgentsRequest struct {
	Updates []json.RawMessage `json:"updates"`
}

// BatchUpdateAgentSkipped names one row that was not applied, and why.
type BatchUpdateAgentSkipped struct {
	AgentID string `json:"agent_id"`
	Reason  string `json:"reason"`
	Error   string `json:"error,omitempty"`
}

// BatchUpdateAgentsResponse is the 200 payload.
//
// Partial success is the contract, not an edge case: pouring 20 agents off a
// rate-limited model must not fail wholesale because one of them belongs to
// another member's private runtime. `updated` and `skipped` are disjoint and
// together cover every submitted `agent_id`.
type BatchUpdateAgentsResponse struct {
	Updated []AgentResponse           `json:"updated"`
	Skipped []BatchUpdateAgentSkipped `json:"skipped"`
}

// BatchUpdateAgents applies a runtime/model re-bucket to many agents in one
// transaction. Drives the Allocator GUI's pour action.
//
// Authorization is per row, not per request: every entry re-runs the same
// manage gate the single-agent dialog uses, so a workspace admin can move
// anyone's agents while a plain member can only move their own. Rows the
// caller may not touch land in `skipped`, never in a 403 that discards the
// rest of the batch.
func (h *Handler) BatchUpdateAgents(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	rawEntries, err := decodeBatchUpdateBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rawEntries) == 0 {
		writeError(w, http.StatusBadRequest, "updates must not be empty")
		return
	}
	if len(rawEntries) > maxBatchUpdateAgents {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many updates: %d exceeds the limit of %d", len(rawEntries), maxBatchUpdateAgents))
		return
	}

	// Validate the whole envelope before taking any lock. A malformed row is a
	// client bug, and a batch that half-applied because of one would be
	// indistinguishable from a partial-success result the client can reason
	// about.
	entries := make([]batchUpdateAgentEntry, 0, len(rawEntries))
	for i, raw := range rawEntries {
		entry, fields, decErr := decodeBatchEntry(raw)
		if decErr != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("updates[%d]: %s", i, decErr.Error()))
			return
		}
		if bad := unknownBatchField(fields); bad != "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("updates[%d]: field %q is not supported by batch-update", i, bad))
			return
		}
		if entry.AgentID == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("updates[%d]: agent_id is required", i))
			return
		}
		entries = append(entries, entry)
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	resp := BatchUpdateAgentsResponse{
		Updated: []AgentResponse{},
		Skipped: []BatchUpdateAgentSkipped{},
	}
	// Keep the post-commit broadcast work aligned with the rows that actually
	// changed; a skipped row must not emit an event.
	type appliedRow struct {
		agent db.Agent
	}
	var applied []appliedRow

	// Dedupe: two entries naming one agent would take the row lock twice and
	// apply the second patch over the first, so the last one silently wins.
	// Refuse rather than guess which the caller meant.
	seen := make(map[string]struct{}, len(entries))

	for i, entry := range entries {
		if _, dup := seen[entry.AgentID]; dup {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("updates[%d]: duplicate agent_id %q", i, entry.AgentID))
			return
		}
		seen[entry.AgentID] = struct{}{}

		agentUUID, parseErr := util.ParseUUID(entry.AgentID)
		if parseErr != nil {
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchReasonNotFound, Error: "agent not found",
			})
			continue
		}

		// Row lock, then the workspace/kind/archived checks. Locking first
		// matters: the checks have to describe the state the update will apply
		// to, not a snapshot read before another writer got in.
		existing, loadErr := qtx.GetAgentForUpdate(r.Context(), agentUUID)
		if errors.Is(loadErr, pgx.ErrNoRows) {
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchReasonNotFound, Error: "agent not found",
			})
			continue
		}
		if loadErr != nil {
			slog.Error("batch update: lock agent failed", append(logger.RequestAttrs(r), "error", loadErr, "agent_id", entry.AgentID)...)
			writeError(w, http.StatusInternalServerError, "failed to update agents")
			return
		}
		if uuidToString(existing.WorkspaceID) != uuidToString(wsUUID) || existing.Kind != "user" {
			// Report as not-found rather than "another workspace": confirming an
			// agent exists somewhere the caller cannot reach is an info leak.
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchReasonNotFound, Error: "agent not found",
			})
			continue
		}
		if existing.ArchivedAt.Valid {
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchReasonArchived, Error: "agent is archived",
			})
			continue
		}
		if !canManageAgentFor(existing, member) {
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchReasonForbidden, Error: "only the agent owner or a workspace owner/admin can manage this agent",
			})
			continue
		}

		params := db.UpdateAgentParams{ID: existing.ID}
		updateReq := UpdateAgentRequest{
			RuntimeID:     entry.RuntimeID,
			Model:         entry.Model,
			ThinkingLevel: entry.ThinkingLevel,
			ServiceTier:   entry.ServiceTier,
		}
		swap, patchErr := h.planAgentRuntimeSwap(r.Context(), member, existing, updateReq, &params)
		if patchErr != nil {
			resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
				AgentID: entry.AgentID, Reason: patchErr.reason, Error: patchErr.msg,
			})
			continue
		}

		updated, updErr := qtx.UpdateAgent(r.Context(), params)
		if updErr != nil {
			// A name collision cannot happen here (this path never writes
			// `name`), but a runtime row deleted between the gate and the UPDATE
			// surfaces as an FK violation, and that one is worth naming.
			var pgErr *pgconn.PgError
			if errors.As(updErr, &pgErr) && pgErr.Code == "23503" {
				resp.Skipped = append(resp.Skipped, BatchUpdateAgentSkipped{
					AgentID: entry.AgentID, Reason: patchReasonRuntimeNotFound, Error: "runtime no longer exists",
				})
				continue
			}
			slog.Error("batch update: agent update failed", append(logger.RequestAttrs(r), "error", updErr, "agent_id", entry.AgentID)...)
			writeError(w, http.StatusInternalServerError, "failed to update agents")
			return
		}
		updated, clearErr := swap.apply(r.Context(), qtx, updated)
		if clearErr != nil {
			slog.Error("batch update: clear failed", append(logger.RequestAttrs(r), "error", clearErr, "agent_id", entry.AgentID)...)
			writeError(w, http.StatusInternalServerError, "failed to update agents")
			return
		}
		applied = append(applied, appliedRow{agent: updated})
	}

	// One audit row for the batch rather than one per agent: the unit of intent
	// is the pour, and per-agent history would bury the rest of the activity
	// feed under a 20-row fan-out.
	if len(applied) > 0 {
		details, _ := json.Marshal(map[string]any{
			"batch_size": len(applied),
			"skipped":    len(resp.Skipped),
		})
		if _, auditErr := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
			ID:          dbid.NewV7(),
			WorkspaceID: wsUUID,
			IssueID:     pgtype.UUID{},
			ActorType:   pgtype.Text{String: "member", Valid: true},
			ActorID:     parseUUID(uuidToString(member.UserID)),
			Action:      "agent_batch_update",
			Details:     details,
		}); auditErr != nil {
			slog.Error("batch update: audit write failed; rolling back", append(logger.RequestAttrs(r), "error", auditErr)...)
			writeError(w, http.StatusInternalServerError, "audit log write failed; batch update rolled back")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("batch update: commit failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update agents")
		return
	}

	// Post-commit: build responses and broadcast, outside the transaction so a
	// slow skills join cannot hold row locks. Publishing per row keeps existing
	// clients working unchanged — they already handle a single-agent status
	// event, so the batch endpoint needs no new realtime code on their side.
	for _, row := range applied {
		respRow, _, _, finErr := h.finalizeAgentMutationResponse(r.Context(), r, row.agent, userID)
		if finErr != nil {
			slog.Warn("batch update: response build failed", append(logger.RequestAttrs(r), "error", finErr, "agent_id", uuidToString(row.agent.ID))...)
			continue
		}
		resp.Updated = append(resp.Updated, respRow)
	}

	slog.Info("agents batch updated",
		append(logger.RequestAttrs(r),
			"workspace_id", workspaceID,
			"updated", len(resp.Updated),
			"skipped", len(resp.Skipped))...)
	writeJSON(w, http.StatusOK, resp)
}

// decodeBatchUpdateBody pulls the `updates` array out of the envelope as raw
// entries so each can be re-decoded alongside its own field-presence map.
func decodeBatchUpdateBody(r *http.Request) ([]json.RawMessage, error) {
	var req BatchUpdateAgentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, errors.New("invalid request body")
	}
	entries := make([]json.RawMessage, 0, len(req.Updates))
	for _, raw := range req.Updates {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			return nil, errors.New("each update must be a JSON object")
		}
		entries = append(entries, raw)
	}
	return entries, nil
}

// decodeBatchEntry decodes one entry and returns the raw field map that lets
// the caller tell "omitted" from "present but null".
func decodeBatchEntry(raw json.RawMessage) (batchUpdateAgentEntry, map[string]json.RawMessage, error) {
	var entry batchUpdateAgentEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return entry, nil, errors.New("invalid update object: " + err.Error())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return entry, nil, errors.New("invalid update object")
	}
	return entry, fields, nil
}

// unknownBatchField returns the first field name outside the accepted set, or
// "" when every field is supported. Detection is order-independent over a map,
// so sort is not needed — the first match is only used to name the offender.
func unknownBatchField(fields map[string]json.RawMessage) string {
	for name := range fields {
		if _, allowed := batchUpdateAllowedFields[name]; !allowed {
			return name
		}
	}
	return ""
}
