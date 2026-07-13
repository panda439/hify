import { useEffect } from "react";
import { BrowserRouter, Routes, Route } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";

import { LoginPage } from "@/routes/login";
import { ProtectedRoute } from "@/routes/protected-route";
import { AppShell } from "@/routes/app-shell";
import { PlaceholderPage } from "@/routes/placeholder-page";
import { ChatPage } from "@/routes/chat";
import { ModelsPage } from "@/routes/models";
import { AgentsPage } from "@/routes/agents";
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
            <Route path="knowledge" element={<PlaceholderPage title="知识库" />} />
            <Route path="mcp" element={<PlaceholderPage title="MCP 工具" />} />
            <Route path="workflows" element={<PlaceholderPage title="工作流" />} />
          </Route>
        </Routes>
      </BrowserRouter>
      <Toaster />
    </QueryClientProvider>
  );
}

export default App;
