"use client";

import { useState, useCallback, useRef, useEffect, useMemo } from "react";
import { Plus, Pencil, Trash2, ChevronDown } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
} from "@multica/ui/components/ui/popover";
import { Input } from "@multica/ui/components/ui/input";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  viewListOptions,
  useCreateView,
  useUpdateView,
  useDeleteView,
} from "@multica/core/views";
import type { SavedView, ListViewsParams } from "@multica/core/types";
import { useT } from "../../i18n";

interface ViewTabsProps {
  page: "issues" | "my_issues" | "project";
  projectId?: string;
  activeViewId: string | null;
  onViewSelect: (view: SavedView) => void;
  isDirty?: boolean;
  onSave?: () => void;
  getCurrentFilters: () => Record<string, unknown>;
  labelOverrides?: Record<string, string>;
}

export function ViewTabs({
  page,
  projectId,
  activeViewId,
  onViewSelect,
  isDirty = false,
  onSave,
  getCurrentFilters,
  labelOverrides,
}: ViewTabsProps) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const params: ListViewsParams = useMemo(
    () => ({ page, ...(projectId ? { project_id: projectId } : {}) }),
    [page, projectId],
  );

  const { data: views = [] } = useQuery(viewListOptions(wsId, params));

  const createView = useCreateView(params);
  const updateView = useUpdateView(params);
  const deleteViewMut = useDeleteView(params);

  const [createOpen, setCreateOpen] = useState(false);
  const [createName, setCreateName] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<SavedView | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const renameInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!activeViewId && views.length > 0) {
      const defaultView = views.find((v) => v.is_default) ?? views[0];
      if (defaultView) onViewSelect(defaultView);
    }
  }, [activeViewId, views, onViewSelect]);

  const handleCreate = useCallback(() => {
    const name = createName.trim();
    if (!name) return;
    createView.mutate(
      {
        name,
        page,
        ...(projectId ? { project_id: projectId } : {}),
        filters: getCurrentFilters(),
      },
      {
        onSuccess: (newView) => {
          setCreateOpen(false);
          setCreateName("");
          onViewSelect(newView);
        },
        onError: () => toast.error(t(($) => $.saved_views.create_failed)),
      },
    );
  }, [createName, page, projectId, createView, onViewSelect, getCurrentFilters, t]);

  const handleRenameSubmit = useCallback(() => {
    if (!renamingId) return;
    const name = renameName.trim();
    if (!name) {
      setRenamingId(null);
      return;
    }
    updateView.mutate(
      { id: renamingId, name },
      {
        onSuccess: () => setRenamingId(null),
        onError: () => toast.error(t(($) => $.saved_views.update_failed)),
      },
    );
  }, [renamingId, renameName, updateView, t]);

  const handleDelete = useCallback(() => {
    if (!deleteTarget) return;
    const targetId = deleteTarget.id;
    deleteViewMut.mutate(targetId, {
      onSuccess: () => {
        setDeleteTarget(null);
        if (targetId === activeViewId) {
          const defaultView = views.find((v) => v.is_default);
          if (defaultView) onViewSelect(defaultView);
        }
      },
      onError: () => toast.error(t(($) => $.saved_views.delete_failed)),
    });
  }, [deleteTarget, deleteViewMut, activeViewId, views, onViewSelect, t]);

  const handleSave = useCallback(() => {
    if (!activeViewId) return;
    updateView.mutate(
      { id: activeViewId, filters: getCurrentFilters() },
      {
        onSuccess: () => onSave?.(),
        onError: () => toast.error(t(($) => $.saved_views.update_failed)),
      },
    );
  }, [activeViewId, updateView, getCurrentFilters, onSave, t]);

  useEffect(() => {
    if (renamingId) renameInputRef.current?.focus();
  }, [renamingId]);

  const getLabel = (view: SavedView) =>
    labelOverrides?.[view.name] ?? view.name;

  const activeView = views.find((v) => v.id === activeViewId);
  const canSave = isDirty && activeView && !activeView.is_default;

  return (
    <>
      <div className="flex items-center gap-1">
        {views.map((view) => {
          const isActive = view.id === activeViewId;

          if (view.id === renamingId) {
            return (
              <Input
                key={view.id}
                ref={renameInputRef}
                value={renameName}
                onChange={(e) => setRenameName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") handleRenameSubmit();
                  if (e.key === "Escape") setRenamingId(null);
                }}
                onBlur={handleRenameSubmit}
                className="h-8 w-28 text-sm"
              />
            );
          }

          return (
            <span key={view.id} className="relative flex items-center">
              <Button
                variant="outline"
                size="sm"
                className={
                  isActive
                    ? "bg-accent text-accent-foreground hover:bg-accent/80"
                    : "text-muted-foreground"
                }
                onClick={() => onViewSelect(view)}
                onDoubleClick={() => {
                  if (!view.is_default) {
                    setRenamingId(view.id);
                    setRenameName(view.name);
                  }
                }}
              >
                {getLabel(view)}
                {isActive && isDirty && (
                  <span className="ml-1 size-1.5 rounded-full bg-primary" />
                )}
              </Button>

              {isActive && !view.is_default && (
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <button className="-ml-1 flex size-6 items-center justify-center rounded text-muted-foreground hover:bg-accent">
                        <ChevronDown className="size-3" />
                      </button>
                    }
                  />
                  <DropdownMenuContent align="start">
                    <DropdownMenuItem
                      onClick={() => {
                        setRenamingId(view.id);
                        setRenameName(view.name);
                      }}
                    >
                      <Pencil className="mr-2 size-4" />
                      {t(($) => $.saved_views.rename_action)}
                    </DropdownMenuItem>
                    <DropdownMenuItem
                      variant="destructive"
                      onClick={() => setDeleteTarget(view)}
                    >
                      <Trash2 className="mr-2 size-4" />
                      {t(($) => $.saved_views.delete_action)}
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              )}
            </span>
          );
        })}

        {canSave && (
          <Button
            variant="outline"
            size="sm"
            className="text-primary"
            onClick={handleSave}
          >
            {t(($) => $.saved_views.save_action)}
          </Button>
        )}

        <Popover open={createOpen} onOpenChange={setCreateOpen}>
          <PopoverTrigger
            render={
              <Button
                variant="ghost"
                size="icon"
                className="size-8 text-muted-foreground"
              >
                <Plus className="size-4" />
              </Button>
            }
          />
          <PopoverContent className="w-60 p-3" align="start">
            <div className="flex flex-col gap-2">
              <Input
                placeholder={t(($) => $.saved_views.create_placeholder)}
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") handleCreate();
                  if (e.key === "Escape") setCreateOpen(false);
                }}
                autoFocus
              />
              <Button
                size="sm"
                onClick={handleCreate}
                disabled={!createName.trim() || createView.isPending}
              >
                {t(($) => $.saved_views.create_action)}
              </Button>
            </div>
          </PopoverContent>
        </Popover>
      </div>

      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.saved_views.delete_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.saved_views.delete_confirm_desc)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>
              {t(($) => $.saved_views.cancel_action)}
            </AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleDelete}>
              {t(($) => $.saved_views.delete_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
