"use client";

import React, { useCallback, useEffect, useRef, useState } from "react";
import { AppPageFrame } from "@/components/layout/AppPageFrame";
import { PageHeader } from "@/components/layout/PageHeader";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { LoadingState } from "@/components/LoadingState";
import { ErrorState } from "@/components/ErrorState";
import {
  listSubstrateActors,
  listSubstrateWorkers,
  SubstrateActor,
  SubstrateWorker,
} from "@/app/actions/substrate";

const POLL_MS = 2000;

function statusBadge(status: string) {
  const variant: Parameters<typeof Badge>[0]["variant"] =
    status === "Running"
      ? "default"
      : status === "Suspended"
      ? "secondary"
      : status === "Resuming" || status === "Suspending"
      ? "outline"
      : "destructive";
  return <Badge variant={variant}>{status}</Badge>;
}

export default function SubstratePage() {
  const [workers, setWorkers] = useState<SubstrateWorker[]>([]);
  const [actors, setActors] = useState<SubstrateActor[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const inFlight = useRef(false);

  const refresh = useCallback(async () => {
    if (inFlight.current) {
      return;
    }
    inFlight.current = true;
    try {
      const [w, a] = await Promise.all([listSubstrateWorkers(), listSubstrateActors()]);
      if (w.error) {
        throw new Error(w.error);
      }
      if (a.error) {
        throw new Error(a.error);
      }
      setWorkers(w.data ?? []);
      setActors(a.data ?? []);
      setError(null);
      setLastUpdated(new Date());
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to fetch substrate state");
    } finally {
      inFlight.current = false;
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    const id = window.setInterval(() => {
      void refresh();
    }, POLL_MS);
    return () => window.clearInterval(id);
  }, [refresh]);

  if (loading) {
    return <LoadingState />;
  }

  if (error && workers.length === 0 && actors.length === 0) {
    return <ErrorState message={error} />;
  }

  const titleId = "substrate-page-title";

  return (
    <AppPageFrame ariaLabelledBy={titleId}>
      <div className="container mx-auto py-8 px-4">
        <PageHeader
          titleId={titleId}
          title="Substrate"
          description={
            <>
              Live view of the substrate worker pool and actors. Updates every {POLL_MS / 1000}s.
              {lastUpdated && (
                <span className="ml-2 text-xs text-muted-foreground">
                  (last update {lastUpdated.toLocaleTimeString()})
                </span>
              )}
              {error && (
                <span className="ml-2 text-xs text-destructive">refresh error: {error}</span>
              )}
            </>
          }
        />

        <div className="grid gap-8">
          <Card>
            <CardHeader>
              <CardTitle>Workers ({workers.length})</CardTitle>
              <CardDescription>
                Worker pods backed by gVisor sandboxes. A worker is FREE when no actor is assigned, otherwise
                it shows the assigned actor.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {workers.length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">
                  No workers registered. Make sure a WorkerPool exists and ate-controller can see it.
                </div>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Worker pod</TableHead>
                      <TableHead>Pool</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Assigned actor</TableHead>
                      <TableHead>IP</TableHead>
                      <TableHead className="text-right">v</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {workers.map((w) => {
                      const assigned = Boolean(w.actorId);
                      const key = `${w.workerNamespace}/${w.workerPod}`;
                      return (
                        <TableRow key={key}>
                          <TableCell className="font-mono text-xs">
                            {w.workerNamespace}/{w.workerPod}
                          </TableCell>
                          <TableCell className="font-mono text-xs">{w.workerPool}</TableCell>
                          <TableCell>
                            <Badge variant={assigned ? "default" : "secondary"}>
                              {assigned ? "ASSIGNED" : "FREE"}
                            </Badge>
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {assigned ? (
                              <>
                                {w.actorNamespace ? `${w.actorNamespace}/` : ""}
                                {w.actorId}
                                {w.actorTemplate && (
                                  <span className="text-muted-foreground"> ({w.actorTemplate})</span>
                                )}
                              </>
                            ) : (
                              <span className="text-muted-foreground">—</span>
                            )}
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {w.ip || <span className="text-muted-foreground">—</span>}
                          </TableCell>
                          <TableCell className="text-right text-xs text-muted-foreground">{w.version}</TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Actors ({actors.length})</CardTitle>
              <CardDescription>
                Workloads checkpointed in substrate. Each actor lives on a worker pod when running and is
                suspended to its snapshot store otherwise.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {actors.length === 0 ? (
                <div className="py-8 text-center text-sm text-muted-foreground">No actors registered.</div>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Actor ID</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Template</TableHead>
                      <TableHead>Worker pod</TableHead>
                      <TableHead>Pod IP</TableHead>
                      <TableHead className="text-right">v</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {actors.map((a) => (
                      <TableRow key={a.actorId}>
                        <TableCell className="font-mono text-xs">{a.actorId}</TableCell>
                        <TableCell>{statusBadge(a.status)}</TableCell>
                        <TableCell className="font-mono text-xs">
                          {a.actorTemplateNamespace ? `${a.actorTemplateNamespace}/` : ""}
                          {a.actorTemplateName ?? ""}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {a.ateomPodName ? (
                            <>
                              {a.ateomPodNamespace ? `${a.ateomPodNamespace}/` : ""}
                              {a.ateomPodName}
                            </>
                          ) : (
                            <span className="text-muted-foreground">—</span>
                          )}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {a.ateomPodIp || <span className="text-muted-foreground">—</span>}
                        </TableCell>
                        <TableCell className="text-right text-xs text-muted-foreground">{a.version}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </AppPageFrame>
  );
}
