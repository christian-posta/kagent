"use server";

import { BaseResponse } from "@/types";
import { fetchApi, createErrorResponse } from "./utils";

export interface SubstrateWorker {
  workerNamespace: string;
  workerPool: string;
  workerPod: string;
  actorNamespace?: string;
  actorTemplate?: string;
  actorId?: string;
  ip?: string;
  version: number;
}

export interface SubstrateActor {
  actorId: string;
  version: number;
  actorTemplateNamespace?: string;
  actorTemplateName?: string;
  status: string;
  ateomPodNamespace?: string;
  ateomPodName?: string;
  ateomPodIp?: string;
  lastSnapshot?: string;
  inProgressSnapshot?: string;
}

export async function listSubstrateWorkers(): Promise<BaseResponse<SubstrateWorker[]>> {
  try {
    const response = await fetchApi<BaseResponse<SubstrateWorker[]>>("/substrate/workers");
    if (!response) {
      throw new Error("Failed to list substrate workers");
    }
    return { message: "ok", data: response.data ?? [] };
  } catch (error) {
    return createErrorResponse<SubstrateWorker[]>(error, "Error listing substrate workers");
  }
}

export async function listSubstrateActors(): Promise<BaseResponse<SubstrateActor[]>> {
  try {
    const response = await fetchApi<BaseResponse<SubstrateActor[]>>("/substrate/actors");
    if (!response) {
      throw new Error("Failed to list substrate actors");
    }
    return { message: "ok", data: response.data ?? [] };
  } catch (error) {
    return createErrorResponse<SubstrateActor[]>(error, "Error listing substrate actors");
  }
}
