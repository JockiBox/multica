"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { createWorkspaceAwareStorage, registerForWorkspaceRehydration } from "../../platform/workspace-storage";
import { defaultStorage } from "../../platform/storage";

interface ActiveViewState {
  issuesActiveViewId: string | null;
  myIssuesActiveViewId: string | null;
  projectActiveViewIds: Record<string, string | null>;
  setIssuesActiveView: (id: string | null) => void;
  setMyIssuesActiveView: (id: string | null) => void;
  setProjectActiveView: (projectId: string, viewId: string | null) => void;
}

export const useActiveViewStore = create<ActiveViewState>()(
  persist(
    (set) => ({
      issuesActiveViewId: null,
      myIssuesActiveViewId: null,
      projectActiveViewIds: {},
      setIssuesActiveView: (id) => set({ issuesActiveViewId: id }),
      setMyIssuesActiveView: (id) => set({ myIssuesActiveViewId: id }),
      setProjectActiveView: (projectId, viewId) =>
        set((state) => ({
          projectActiveViewIds: { ...state.projectActiveViewIds, [projectId]: viewId },
        })),
    }),
    {
      name: "multica_active_view",
      storage: createJSONStorage(() => createWorkspaceAwareStorage(defaultStorage)),
    },
  ),
);

registerForWorkspaceRehydration(() => useActiveViewStore.persist.rehydrate());
