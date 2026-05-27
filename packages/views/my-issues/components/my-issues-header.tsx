"use client";

import { useCallback, useMemo, useState } from "react";
import { useStore } from "zustand";
import type { Issue, IssueStatus, IssuePriority, SavedView } from "@multica/core/types";
import { myIssuesViewStore, type MyIssuesScope } from "@multica/core/issues/stores/my-issues-view-store";
import {
  useActiveViewStore,
  deserializeViewFilters,
  serializeViewFilters,
  isViewDirty,
} from "@multica/core/views";
import { useT } from "../../i18n";
import { WorkspaceAgentWorkingChip } from "../../issues/components/workspace-agent-working-chip";
import { IssueDisplayControls } from "../../issues/components/issues-header";
import { ViewTabs } from "../../issues/components/view-tabs";

export function MyIssuesHeader({ allIssues }: { allIssues: Issue[] }) {
  const { t } = useT("my-issues");
  const { t: tIssues } = useT("issues");

  const scope = useStore(myIssuesViewStore, (s) => s.scope);
  const agentRunningFilter = useStore(myIssuesViewStore, (s) => s.agentRunningFilter);
  const statusFilters = useStore(myIssuesViewStore, (s) => s.statusFilters);
  const priorityFilters = useStore(myIssuesViewStore, (s) => s.priorityFilters);
  const assigneeFilters = useStore(myIssuesViewStore, (s) => s.assigneeFilters);
  const includeNoAssignee = useStore(myIssuesViewStore, (s) => s.includeNoAssignee);
  const creatorFilters = useStore(myIssuesViewStore, (s) => s.creatorFilters);
  const projectFilters = useStore(myIssuesViewStore, (s) => s.projectFilters);
  const includeNoProject = useStore(myIssuesViewStore, (s) => s.includeNoProject);
  const labelFilters = useStore(myIssuesViewStore, (s) => s.labelFilters);
  const act = myIssuesViewStore.getState();

  const scopedIssueIds = useMemo(
    () => new Set(allIssues.map((i) => i.id)),
    [allIssues],
  );

  const activeViewId = useActiveViewStore((s) => s.myIssuesActiveViewId);
  const setActiveView = useActiveViewStore((s) => s.setMyIssuesActiveView);

  const [savedView, setSavedView] = useState<SavedView | null>(null);

  const isDirty = useMemo(() => {
    if (!savedView) return false;
    return isViewDirty(savedView, scope, {
      statusFilters, priorityFilters, assigneeFilters, includeNoAssignee,
      creatorFilters, projectFilters, includeNoProject, labelFilters,
    });
  }, [savedView, scope, statusFilters, priorityFilters, assigneeFilters, includeNoAssignee, creatorFilters, projectFilters, includeNoProject, labelFilters]);

  const handleViewSelect = useCallback(
    (view: SavedView) => {
      setActiveView(view.id);
      setSavedView(view);
      const deserialized = deserializeViewFilters(view);
      act.setScope(deserialized.scope as MyIssuesScope);
      act.clearFilters();
      const f = deserialized.filters;
      for (const s of f.statusFilters) act.toggleStatusFilter(s as IssueStatus);
      for (const p of f.priorityFilters) act.togglePriorityFilter(p as IssuePriority);
      for (const a of f.assigneeFilters) act.toggleAssigneeFilter(a);
      if (f.includeNoAssignee) act.toggleNoAssignee();
      for (const c of f.creatorFilters) act.toggleCreatorFilter(c);
      for (const p of f.projectFilters) act.toggleProjectFilter(p);
      if (f.includeNoProject) act.toggleNoProject();
      for (const l of f.labelFilters) act.toggleLabelFilter(l);
    },
    [setActiveView, act],
  );

  const getCurrentFilters = useCallback(
    () =>
      serializeViewFilters(scope, {
        statusFilters, priorityFilters, assigneeFilters, includeNoAssignee,
        creatorFilters, projectFilters, includeNoProject, labelFilters,
      }),
    [scope, statusFilters, priorityFilters, assigneeFilters, includeNoAssignee, creatorFilters, projectFilters, includeNoProject, labelFilters],
  );

  const labelOverrides = useMemo(
    () => ({
      All: t(($) => $.header.scope.all_label),
      Assigned: t(($) => $.header.scope.assigned_label),
      Created: t(($) => $.header.scope.created_label),
      "My Agents": t(($) => $.header.scope.agents_label),
    }),
    [t],
  );

  return (
    <div className="flex h-12 shrink-0 items-center justify-between px-4">
      <ViewTabs
        page="my_issues"
        activeViewId={activeViewId}
        onViewSelect={handleViewSelect}
        isDirty={isDirty}
        onSave={() => setSavedView(null)}
        getCurrentFilters={getCurrentFilters}
        labelOverrides={labelOverrides}
      />

      <div className="flex items-center gap-1">
        {agentRunningFilter && (
          <span className="mr-1 text-xs text-muted-foreground">
            {tIssues(($) => $.agent_activity.filter_active_label)}
          </span>
        )}
        <WorkspaceAgentWorkingChip
          value={agentRunningFilter}
          onToggle={act.toggleAgentRunningFilter}
          scopedIssueIds={scopedIssueIds}
        />
        <IssueDisplayControls scopedIssues={allIssues} />
      </div>
    </div>
  );
}
