"use client";
import React from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Plus } from "lucide-react";
import Link from "next/link";

// Dummy data for now
const simulations = [
  {
    id: 1,
    name: "Agent Performance Test",
    description: "End-to-end evaluation of agent performance",
    evaluationType: "end-to-end",
    status: "completed",
    createdAt: "2024-03-20",
  },
  {
    id: 2,
    name: "Trajectory Analysis",
    description: "Analysis of agent decision paths",
    evaluationType: "trajectory",
    status: "running",
    createdAt: "2024-03-21",
  },
];

export default function SimulationsListPage() {
  return (
    <div className="mt-12 mx-auto max-w-6xl px-6">
      <div className="flex justify-between items-center mb-8">
        <div className="flex items-center gap-4">
          <h1 className="text-2xl font-bold">Simulations</h1>
        </div>
        <Button asChild>
          <Link href="/simulations/new">
            <Plus className="h-4 w-4 mr-2" />
            New Simulation
          </Link>
        </Button>
      </div>

      {simulations.length === 0 ? (
        <div className="text-center py-12">
          <h3 className="text-lg font-medium mb-2">No simulations yet</h3>
          <p className="mb-6">Create your first simulation to get started</p>
          <Button asChild>
            <Link href="/simulations/new">
              <Plus className="h-4 w-4 mr-2" />
              Create New Simulation
            </Link>
          </Button>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {simulations.map((simulation) => (
            <Link key={simulation.id} href={`/simulations/${simulation.id}`}>
              <Card className="hover:bg-accent/50 transition-colors">
                <CardHeader>
                  <CardTitle className="text-lg">{simulation.name}</CardTitle>
                </CardHeader>
                <CardContent>
                  <p className="text-sm text-muted-foreground mb-2">
                    {simulation.description}
                  </p>
                  <div className="flex justify-between text-sm">
                    <span className="capitalize">{simulation.evaluationType}</span>
                    <span className="capitalize">{simulation.status}</span>
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
} 