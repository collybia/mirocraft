import { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes } from "react-router-dom";

import { Layout } from "./components/Layout";
import * as api from "./lib/api";
import { Admin } from "./pages/Admin";
import { Login } from "./pages/Login";
import { ServerDetail } from "./pages/ServerDetail";
import { Servers } from "./pages/Servers";
import { Settings } from "./pages/Settings";
import { ThemeProvider } from "./ThemeProvider";

type AuthState = "checking" | "anonymous" | "authenticated";

export function App() {
  const [state, setState] = useState<AuthState>("checking");
  const [user, setUser] = useState<api.Me | null>(null);

  const loadUser = useCallback(async () => {
    if (!api.getToken()) {
      setState("anonymous");
      return;
    }
    try {
      setUser(await api.me());
      setState("authenticated");
    } catch {
      // An expired or revoked token looks the same as no token at all.
      api.setToken(null);
      setState("anonymous");
    }
  }, []);

  useEffect(() => {
    void loadUser();
  }, [loadUser]);

  return (
    <ThemeProvider serverChoice={user?.theme} authenticated={state === "authenticated"}>
      {state === "checking" ? (
        <div className="flex min-h-screen items-center justify-center bg-surface text-muted">
          Загрузка…
        </div>
      ) : state === "anonymous" || !user ? (
        <Routes>
          <Route path="/login" element={<Login onLoggedIn={() => void loadUser()} />} />
          <Route path="*" element={<Navigate to="/login" replace />} />
        </Routes>
      ) : (
        <Layout
          user={user}
          onLoggedOut={() => {
            setUser(null);
            setState("anonymous");
          }}
        >
          <Routes>
            <Route path="/" element={<Servers />} />
            <Route path="/servers/:id" element={<ServerDetail />} />
            <Route path="/settings" element={<Settings user={user} />} />
            {/* Routed only for administrators. The API refuses these calls
                anyway, but a page that loads and then fails on every request
                reads as broken rather than as not-for-you. */}
            {user.role === "admin" && <Route path="/admin" element={<Admin me={user} />} />}
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </Layout>
      )}
    </ThemeProvider>
  );
}
