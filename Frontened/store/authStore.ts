import { create } from "zustand";

export interface AuthUser {
  walletAddress: string;
  isAdmin: boolean;
}

interface AuthState {
  token: string | null;
  expiresAt: string | null;
  user: AuthUser | null;
  setSession: (session: { token: string; expiresAt: string; user: AuthUser }) => void;
  clearSession: () => void;
}

export const useAuthStore = create<AuthState>((set) => ({
  token: null,
  expiresAt: null,
  user: null,
  setSession: ({ token, expiresAt, user }) => set({ token, expiresAt, user }),
  clearSession: () => set({ token: null, expiresAt: null, user: null }),
}));
