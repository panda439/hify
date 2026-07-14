import { useEffect } from "react";
import { BrowserRouter, Routes, Route } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";

import { LoginPage } from "@/routes/login";
import { ProtectedRoute } from "@/routes/protected-route";
import { AppShell } from "@/routes/app-shell";
import { ChatPage } from "@/routes/chat";
import { ModelsPage } from "@/routes/models";
import { AgentsPage } from "@/routes/agents";
import { KnowledgePage } from "@/routes/knowledge";
import { McpPage } from "@/routes/mcp";
import { WorkflowsPage } from "@/routes/workflows";
import { WorkflowEditorPage } from "@/routes/workflow-editor";
import { restoreSession } from "@/lib/session";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

function App() {
  useEffect(() => {
    void restoreSession();
  }, []);

  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route
            element={
              <ProtectedRoute>
                <AppShell />
              </ProtectedRoute>
            }
          >
            <Route index element={<ChatPage />} />
            <Route path="models" element={<ModelsPage />} />
            <Route path="agents" element={<AgentsPage />} />
            <Route path="knowledge" element={<KnowledgePage />} />
            <Route path="mcp" element={<McpPage />} />
            <Route path="workflows" element={<WorkflowsPage />} />
            <Route path="workflows/new" element={<WorkflowEditorPage />} />
            <Route path="workflows/:id" element={<WorkflowEditorPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
      <Toaster />
    </QueryClientProvider>
  );
}

export default App;
