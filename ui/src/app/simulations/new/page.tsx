"use client";
import React, { useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Label } from "@/components/ui/label";
import { Loader2 } from "lucide-react";

export default function NewSimulationPage() {
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [formData, setFormData] = useState({
    name: "",
    description: "",
    evaluationType: "",
    agentId: "",
    testCases: "",
    metrics: [],
  });

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setIsSubmitting(true);
    // TODO: Implement simulation creation
    setTimeout(() => setIsSubmitting(false), 1000);
  };

  return (
    <div className="min-h-screen p-8">
      <div className="max-w-6xl mx-auto">
        <h1 className="text-2xl font-bold mb-8">Create New Simulation</h1>

        <form onSubmit={handleSubmit} className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle>Basic Information</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              <div>
                <Label htmlFor="name">Simulation Name</Label>
                <Input
                  id="name"
                  value={formData.name}
                  onChange={(e) => setFormData({ ...formData, name: e.target.value })}
                  placeholder="Enter simulation name..."
                />
              </div>

              <div>
                <Label htmlFor="description">Description</Label>
                <Textarea
                  id="description"
                  value={formData.description}
                  onChange={(e) => setFormData({ ...formData, description: e.target.value })}
                  placeholder="Describe your simulation..."
                  className="min-h-[100px]"
                />
              </div>

              <div>
                <Label htmlFor="evaluationType">Evaluation Type</Label>
                <Select
                  value={formData.evaluationType}
                  onValueChange={(value) => setFormData({ ...formData, evaluationType: value })}
                >
                  <SelectTrigger>
                    <SelectValue placeholder="Select evaluation type" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="end-to-end">End-to-End Evaluation</SelectItem>
                    <SelectItem value="trajectory">Trajectory Analysis</SelectItem>
                    <SelectItem value="single-step">Single Step Evaluation</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              <div>
                <Label htmlFor="agentId">Target Agent</Label>
                <Select
                  value={formData.agentId}
                  onValueChange={(value) => setFormData({ ...formData, agentId: value })}
                >
                  <SelectTrigger>
                    <SelectValue placeholder="Select agent" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="agent1">Agent 1</SelectItem>
                    <SelectItem value="agent2">Agent 2</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              <div>
                <Label htmlFor="testCases">Test Cases</Label>
                <Textarea
                  id="testCases"
                  value={formData.testCases}
                  onChange={(e) => setFormData({ ...formData, testCases: e.target.value })}
                  placeholder="Enter test cases (one per line)..."
                  className="min-h-[150px]"
                />
              </div>
            </CardContent>
          </Card>

          <div className="flex justify-end">
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? (
                <>
                  <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                  Creating...
                </>
              ) : (
                "Create Simulation"
              )}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
} 