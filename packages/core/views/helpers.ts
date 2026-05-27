import type { SavedView } from "../types";
import type { IssueViewState, ActorFilterValue } from "../issues/stores/view-store";

type IssuesScope = "all" | "members" | "agents";
type MyIssuesScope = "all" | "assigned" | "created" | "agents";

interface FilterSnapshot {
  statusFilters: string[];
  priorityFilters: string[];
  assigneeFilters: ActorFilterValue[];
  includeNoAssignee: boolean;
  creatorFilters: ActorFilterValue[];
  projectFilters: string[];
  includeNoProject: boolean;
  labelFilters: string[];
}

const EMPTY_FILTERS: FilterSnapshot = {
  statusFilters: [],
  priorityFilters: [],
  assigneeFilters: [],
  includeNoAssignee: false,
  creatorFilters: [],
  projectFilters: [],
  includeNoProject: false,
  labelFilters: [],
};

// Map default view names to scopes (used for is_default views)
const ISSUES_NAME_TO_SCOPE: Record<string, IssuesScope> = {
  All: "all",
  Members: "members",
  Agents: "agents",
};

const MY_ISSUES_NAME_TO_SCOPE: Record<string, MyIssuesScope> = {
  All: "all",
  Assigned: "assigned",
  Created: "created",
  "My Agents": "agents",
};

export interface DeserializedView {
  scope: string;
  filters: FilterSnapshot;
}

/**
 * Given a SavedView, extract the scope and filter state.
 * Default views: scope derived from name, filters empty.
 * Custom views: scope + filters read from __custom key in filters JSON.
 */
export function deserializeViewFilters(view: SavedView): DeserializedView {
  // Custom view with __custom key
  const raw = view.filters as Record<string, unknown>;
  if (raw.__custom && typeof raw.__custom === "object") {
    const custom = raw.__custom as Record<string, unknown>;
    return {
      scope: (custom.scope as string) ?? "all",
      filters: {
        statusFilters: Array.isArray(custom.statusFilters) ? custom.statusFilters : [],
        priorityFilters: Array.isArray(custom.priorityFilters) ? custom.priorityFilters : [],
        assigneeFilters: Array.isArray(custom.assigneeFilters) ? custom.assigneeFilters : [],
        includeNoAssignee: custom.includeNoAssignee === true,
        creatorFilters: Array.isArray(custom.creatorFilters) ? custom.creatorFilters : [],
        projectFilters: Array.isArray(custom.projectFilters) ? custom.projectFilters : [],
        includeNoProject: custom.includeNoProject === true,
        labelFilters: Array.isArray(custom.labelFilters) ? custom.labelFilters : [],
      },
    };
  }

  // Default view: derive scope from name
  if (view.page === "issues") {
    return {
      scope: ISSUES_NAME_TO_SCOPE[view.name] ?? "all",
      filters: EMPTY_FILTERS,
    };
  }
  if (view.page === "my_issues") {
    return {
      scope: MY_ISSUES_NAME_TO_SCOPE[view.name] ?? "all",
      filters: EMPTY_FILTERS,
    };
  }
  // Project page: no scope concept
  return { scope: "all", filters: EMPTY_FILTERS };
}

/**
 * Serialize current scope + filter state into filters JSON for the API.
 * Stores under __custom key so we can distinguish from default view filters.
 */
export function serializeViewFilters(
  scope: string,
  state: Pick<
    IssueViewState,
    | "statusFilters"
    | "priorityFilters"
    | "assigneeFilters"
    | "includeNoAssignee"
    | "creatorFilters"
    | "projectFilters"
    | "includeNoProject"
    | "labelFilters"
  >,
): Record<string, unknown> {
  return {
    __custom: {
      scope,
      statusFilters: state.statusFilters,
      priorityFilters: state.priorityFilters,
      assigneeFilters: state.assigneeFilters,
      includeNoAssignee: state.includeNoAssignee,
      creatorFilters: state.creatorFilters,
      projectFilters: state.projectFilters,
      includeNoProject: state.includeNoProject,
      labelFilters: state.labelFilters,
    },
  };
}

/**
 * Compare current filter state with a saved view's filters.
 * Returns true if they differ (view is "dirty").
 */
export function isViewDirty(
  view: SavedView,
  currentScope: string,
  currentState: Pick<
    IssueViewState,
    | "statusFilters"
    | "priorityFilters"
    | "assigneeFilters"
    | "includeNoAssignee"
    | "creatorFilters"
    | "projectFilters"
    | "includeNoProject"
    | "labelFilters"
  >,
): boolean {
  const saved = deserializeViewFilters(view);
  if (saved.scope !== currentScope) return true;
  const sf = saved.filters;
  if (JSON.stringify(sf.statusFilters) !== JSON.stringify(currentState.statusFilters)) return true;
  if (JSON.stringify(sf.priorityFilters) !== JSON.stringify(currentState.priorityFilters)) return true;
  if (JSON.stringify(sf.assigneeFilters) !== JSON.stringify(currentState.assigneeFilters)) return true;
  if (sf.includeNoAssignee !== currentState.includeNoAssignee) return true;
  if (JSON.stringify(sf.creatorFilters) !== JSON.stringify(currentState.creatorFilters)) return true;
  if (JSON.stringify(sf.projectFilters) !== JSON.stringify(currentState.projectFilters)) return true;
  if (sf.includeNoProject !== currentState.includeNoProject) return true;
  if (JSON.stringify(sf.labelFilters) !== JSON.stringify(currentState.labelFilters)) return true;
  return false;
}
