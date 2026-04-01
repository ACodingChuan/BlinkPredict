import { create } from "zustand";

export type LinkedWallet = {
  id: string;
  address: string;
  chainType: string;
  connectorType: string;
  type: "wallet";
};

export type LinkedAccount = LinkedWallet;

export interface User {
  id: string;
  linkedAccounts: LinkedAccount[];
}

interface UserStore {
  user: User | null;
  setUser: (user: User | null) => void;
  logout: () => void;
}

export const useUserStore = create<UserStore>((set) => ({
  user: null,
  setUser: (user) => set({ user }),
  logout: () => set({ user: null }),
}));
