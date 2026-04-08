import { create } from "zustand";

interface TradingAccountState {
  walletAddress?: string;
  tradingTotal: string;
  tradingAvailable: string;
  tradingLocked: string;
  tradingPending: string;
  loading: boolean;
  setLoading: (loading: boolean) => void;
  setSnapshot: (payload: {
    walletAddress: string;
    tradingTotal: string;
    tradingAvailable: string;
    tradingLocked: string;
    tradingPending: string;
  }) => void;
  clearSnapshot: () => void;
}

const EMPTY_BALANCE = "0.00";

export const useTradingAccountStore = create<TradingAccountState>((set) => ({
  walletAddress: undefined,
  tradingTotal: EMPTY_BALANCE,
  tradingAvailable: EMPTY_BALANCE,
  tradingLocked: EMPTY_BALANCE,
  tradingPending: EMPTY_BALANCE,
  loading: false,
  setLoading: (loading) => set({ loading }),
  setSnapshot: ({ walletAddress, tradingTotal, tradingAvailable, tradingLocked, tradingPending }) =>
    set({
      walletAddress,
      tradingTotal,
      tradingAvailable,
      tradingLocked,
      tradingPending,
      loading: false,
    }),
  clearSnapshot: () =>
    set({
      walletAddress: undefined,
      tradingTotal: EMPTY_BALANCE,
      tradingAvailable: EMPTY_BALANCE,
      tradingLocked: EMPTY_BALANCE,
      tradingPending: EMPTY_BALANCE,
      loading: false,
    }),
}));
