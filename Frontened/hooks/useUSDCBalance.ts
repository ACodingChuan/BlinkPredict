import { useCallback, useEffect, useRef } from "react";
import { usePrivy } from "@/lib/auth-client";
import api from "@/app/utils/axiosInstance";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import { useTradingAccountStore } from "@/store/tradingAccountStore";

type WalletAccountResponse = {
  collateral_total_units: string;
  collateral_free_units: string;
  collateral_locked_units: string;
  collateral_pending_units: string;
};

const MUTATION_SYNC_DELAYS_MS = [0, 400, 1200, 2500];

export const useUSDCBalance = (options?: { autoFetch?: boolean }) => {
  const autoFetch = options?.autoFetch ?? true;
  const { walletAddress } = useCurrentSolanaWallet();
  const { authenticated, getAccessToken } = usePrivy();
  const storedWalletAddress = useTradingAccountStore((current) => current.walletAddress);
  const tradingTotal = useTradingAccountStore((current) => current.tradingTotal);
  const tradingAvailable = useTradingAccountStore((current) => current.tradingAvailable);
  const tradingLocked = useTradingAccountStore((current) => current.tradingLocked);
  const tradingPending = useTradingAccountStore((current) => current.tradingPending);
  const loading = useTradingAccountStore((current) => current.loading);
  const setLoading = useTradingAccountStore((current) => current.setLoading);
  const setSnapshot = useTradingAccountStore((current) => current.setSnapshot);
  const clearSnapshot = useTradingAccountStore((current) => current.clearSnapshot);
  const syncJobRef = useRef(0);

  const fetchBalance = useCallback(async () => {
    if (!walletAddress || !authenticated) {
      clearSnapshot();
      return false;
    }
    const token = await getAccessToken();
    if (!token) {
      setLoading(false);
      return false;
    }
    try {
      const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
        headers: { Authorization: `Bearer ${token}` },
      });
      setSnapshot({
        walletAddress,
        tradingTotal: formatUnits(data.collateral_total_units),
        tradingAvailable: formatUnits(data.collateral_free_units),
        tradingLocked: formatUnits(data.collateral_locked_units),
        tradingPending: formatUnits(data.collateral_pending_units),
      });
    } catch {
      setSnapshot({
        walletAddress,
        tradingTotal: "0.00",
        tradingAvailable: "0.00",
        tradingLocked: "0.00",
        tradingPending: "0.00",
      });
    }
    return true;
  }, [authenticated, clearSnapshot, getAccessToken, setLoading, setSnapshot, walletAddress]);

  useEffect(() => {
    if (!autoFetch) {
      return;
    }
    setLoading(true);
    const timer = setTimeout(() => {
      void fetchBalance();
    }, 0);
    return () => clearTimeout(timer);
  }, [autoFetch, fetchBalance, setLoading]);

  return {
    balance: !walletAddress || !authenticated || storedWalletAddress !== walletAddress ? "0.00" : tradingAvailable,
    availableBalance: !walletAddress || !authenticated || storedWalletAddress !== walletAddress ? "0.00" : tradingAvailable,
    totalBalance: !walletAddress || !authenticated || storedWalletAddress !== walletAddress ? "0.00" : tradingTotal,
    lockedBalance: !walletAddress || !authenticated || storedWalletAddress !== walletAddress ? "0.00" : tradingLocked,
    pendingBalance: !walletAddress || !authenticated || storedWalletAddress !== walletAddress ? "0.00" : tradingPending,
    loading: loading || (Boolean(walletAddress) && authenticated && storedWalletAddress !== walletAddress),
    walletAddress,
    refetch: async () => {
      setLoading(true);
      await fetchBalance();
    },
    syncAfterMutation: async (delaysMs?: number[]) => {
      const jobID = ++syncJobRef.current;
      const schedule = Array.isArray(delaysMs) && delaysMs.length > 0 ? delaysMs : MUTATION_SYNC_DELAYS_MS;
      for (const delayMs of schedule) {
        if (delayMs > 0) {
          await sleep(delayMs);
        }
        if (jobID !== syncJobRef.current) {
          return;
        }
        await fetchBalance();
      }
    },
  };
};

function sleep(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}

function formatUnits(raw: string): string {
  const parsed = Number(raw || "0");
  if (!Number.isFinite(parsed)) return "0.00";
  return (parsed / 100).toFixed(2);
}
