import { useCallback, useEffect, useRef, useState } from "react";
import { usePrivy } from "@/lib/auth-client";
import api from "@/app/utils/axiosInstance";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";

type WalletAccountResponse = {
  collateral_total_units: string;
  collateral_free_units: string;
};

type BalanceState = {
  walletAddress?: string;
  tradingTotal: string;
  tradingAvailable: string;
  loading: boolean;
};

const MUTATION_SYNC_DELAYS_MS = [0, 400, 1200, 2500];

export const useUSDCBalance = () => {
  const { walletAddress } = useCurrentSolanaWallet();
  const { authenticated, getAccessToken } = usePrivy();
  const [state, setState] = useState<BalanceState>({
    tradingTotal: "0.00",
    tradingAvailable: "0.00",
    loading: false,
  });
  const syncJobRef = useRef(0);

  const fetchBalance = useCallback(async () => {
    if (!walletAddress || !authenticated) {
      setState((current) => ({ ...current, loading: false }));
      return false;
    }
    const token = await getAccessToken();
    if (!token) {
      setState((current) => ({ ...current, loading: false }));
      return false;
    }
    try {
      const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
        headers: { Authorization: `Bearer ${token}` },
      });
      setState((current) => ({
        ...current,
        walletAddress,
        tradingTotal: formatUnits(data.collateral_total_units),
        tradingAvailable: formatUnits(data.collateral_free_units),
        loading: false,
      }));
    } catch {
      setState((current) => ({
        ...current,
        walletAddress,
        tradingTotal: "0.00",
        tradingAvailable: "0.00",
        loading: false,
      }));
    }
    return true;
  }, [authenticated, getAccessToken, walletAddress]);

  useEffect(() => {
    const timer = setTimeout(() => {
      void fetchBalance();
    }, 0);
    return () => clearTimeout(timer);
  }, [fetchBalance]);

  return {
    balance: !walletAddress || !authenticated || state.walletAddress !== walletAddress ? "0.00" : state.tradingAvailable,
    totalBalance: !walletAddress || !authenticated || state.walletAddress !== walletAddress ? "0.00" : state.tradingTotal,
    loading: state.loading || (Boolean(walletAddress) && authenticated && state.walletAddress !== walletAddress),
    walletAddress,
    refetch: async () => {
      setState((current) => ({ ...current, loading: true }));
      await fetchBalance();
    },
    syncAfterMutation: async () => {
      const jobID = ++syncJobRef.current;
      for (const delayMs of MUTATION_SYNC_DELAYS_MS) {
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
