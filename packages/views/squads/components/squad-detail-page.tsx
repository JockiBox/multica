"use client";

import { useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useCurrentWorkspace } from "@multica/core/paths";
import { useNavigation } from "../../navigation";
import { PageHeader } from "../../layout/page-header";
import { Users } from "lucide-react";
import type { Squad, SquadMember } from "@multica/core/types";

export function SquadDetailPage() {
  const workspace = useCurrentWorkspace();
  const { pathname } = useNavigation();
  const squadId = pathname.split("/").pop() ?? "";

  const { data: squad } = useQuery<Squad>({
    queryKey: ["squad", workspace?.id, squadId],
    queryFn: () => api.get(`/api/squads/${squadId}`),
    enabled: !!workspace?.id && !!squadId,
  });

  const { data: members = [] } = useQuery<SquadMember[]>({
    queryKey: ["squad-members", workspace?.id, squadId],
    queryFn: () => api.get(`/api/squads/${squadId}/members`),
    enabled: !!workspace?.id && !!squadId,
  });

  if (!squad) {
    return <div className="p-6 text-muted-foreground text-sm">Loading...</div>;
  }

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader className="justify-between px-5">
        <div className="flex items-center gap-2">
          <Users className="h-4 w-4 text-muted-foreground" />
          <h1 className="text-sm font-medium">{squad.name}</h1>
        </div>
      </PageHeader>
      <div className="flex-1 p-6 space-y-6">
        {squad.description && (
          <p className="text-sm text-muted-foreground">{squad.description}</p>
        )}
        <div>
          <h3 className="text-sm font-medium mb-3">Members ({members.length})</h3>
          <div className="grid gap-2">
            {members.map((m) => (
              <div key={m.id} className="flex items-center gap-3 rounded-md border p-3">
                <Users className="size-4 text-muted-foreground" />
                <div className="flex-1 min-w-0">
                  <span className="text-sm">{m.member_type}: {m.member_id}</span>
                  {m.role && <span className="ml-2 text-xs text-muted-foreground">({m.role})</span>}
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}
