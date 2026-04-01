"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { useWallet } from "@solana/wallet-adapter-react";
import { useWalletModal } from "@solana/wallet-adapter-react-ui";
import { Buffer } from "buffer";
import { toast } from "sonner";
import { useAuthStore, type AuthUser } from "@/store/authStore";

const STORAGE_KEY = "blinkpredict.auth.session";

type AuthContextValue = {
  ready: boolean;
  authenticated: boolean;
  user: AuthUser | null;
  login: () => Promise<void>;
  logout: () => Promise<void>;
  getAccessToken: () => Promise<string | null>;
  identityToken: string | null;
};

const AuthContext = createContext<AuthContextValue | null>(null);

function readStoredSession() {
  if (typeof window === "undefined") return null;
  const raw = window.localStorage.getItem(STORAGE_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as { token: string; expiresAt: string; user: AuthUser };
  } catch {
    return null;
  }
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const { connected, publicKey, signMessage, disconnect, connecting } = useWallet();
  const { setVisible } = useWalletModal();
  const { token, expiresAt, user, setSession, clearSession } = useAuthStore();
  const [ready] = useState(true);
  const walletAddress = publicKey?.toBase58() ?? null;

  const persistSession = useCallback((session: { token: string; expiresAt: string; user: AuthUser } | null) => {
    if (typeof window === "undefined") return;
    if (!session) {
      window.localStorage.removeItem(STORAGE_KEY);
      return;
    }
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(session));
  }, []);

  useEffect(() => {
    const session = readStoredSession();
    if (session && new Date(session.expiresAt).getTime() > Date.now()) {
      setSession(session);
    }
  }, [setSession]);

  useEffect(() => {
    if (token) {
      api.defaults.headers.common.Authorization = `Bearer ${token}`;
    } else {
      delete api.defaults.headers.common.Authorization;
    }
  }, [token]);

  useEffect(() => {
    if (!walletAddress && token) {
      clearSession();
      persistSession(null);
    }
  }, [walletAddress, token, clearSession, persistSession]);

  const login = useCallback(async () => {
    if (!walletAddress) {
      setVisible(true);
      return;
    }
    if (!signMessage) {
      toast.error("Connected wallet does not support message signing");
      return;
    }

    const existing = readStoredSession();
    if (existing && existing.user.walletAddress === walletAddress && new Date(existing.expiresAt).getTime() > Date.now()) {
      setSession(existing);
      return;
    }

    const { data: challenge } = await api.post<{ challenge_id: string; message: string; expires_at: string }>("/auth/challenge", {
      wallet_address: walletAddress,
    });
    const signed = await signMessage(new TextEncoder().encode(challenge.message));
    const signature = Buffer.from(signed).toString("base64");
    const { data } = await api.post<{ auth_token: string; expires_at: string; user: AuthUser }>("/auth/login", {
      wallet_address: walletAddress,
      challenge_id: challenge.challenge_id,
      signature,
    });
    const session = { token: data.auth_token, expiresAt: data.expires_at, user: data.user };
    setSession(session);
    persistSession(session);
  }, [walletAddress, signMessage, setSession, persistSession, setVisible]);

  useEffect(() => {
    if (!ready || connecting || !connected || !walletAddress) return;
    if (!token || user?.walletAddress !== walletAddress || (expiresAt && new Date(expiresAt).getTime() <= Date.now())) {
      void login().catch((error: unknown) => {
        const message = error instanceof Error ? error.message : "Wallet login failed";
        toast.error(message);
      });
    }
  }, [ready, connecting, connected, walletAddress, token, user, expiresAt, login]);

  const logout = useCallback(async () => {
    clearSession();
    persistSession(null);
    try {
      await disconnect();
    } catch {
      // noop
    }
  }, [clearSession, persistSession, disconnect]);

  const getAccessToken = useCallback(async () => {
    if (!token || !expiresAt) return null;
    if (new Date(expiresAt).getTime() <= Date.now()) {
      clearSession();
      persistSession(null);
      return null;
    }
    return token;
  }, [token, expiresAt, clearSession, persistSession]);

  const value = useMemo<AuthContextValue>(() => ({
    ready,
    authenticated: Boolean(token && user),
    user,
    login,
    logout,
    getAccessToken,
    identityToken: token,
  }), [ready, token, user, login, logout, getAccessToken]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuthSession() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuthSession must be used within AuthProvider");
  }
  return ctx;
}
