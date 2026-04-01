import { useCallback, useEffect, useState } from "react";
import { usePrivy } from "@/lib/auth-client";
import api from "@/app/utils/axiosInstance";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";

type WalletAccountResponse = {
  collateral_total_units: string;
  collateral_free_units: string;
};

export const useUSDCBalance = () => {
  const { walletAddress } = useCurrentSolanaWallet();
  const { authenticated, getAccessToken } = usePrivy();
  const [tradingTotal, setTradingTotal] = useState("0.00");
  const [tradingAvailable, setTradingAvailable] = useState("0.00");
  const [loading, setLoading] = useState(false);

  const fetchBalance = useCallback(async () => {
    if (!walletAddress || !authenticated) return;
    const token = await getAccessToken();
    if (!token) return;
    try {
      const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
        headers: { Authorization: `Bearer ${token}` },
      });
      setTradingTotal(formatUnits(data.collateral_total_units));
      setTradingAvailable(formatUnits(data.collateral_free_units));
    } catch {
      setTradingTotal("0.00");
      setTradingAvailable("0.00");
    }
  }, [authenticated, getAccessToken, walletAddress]);

  useEffect(() => {
    const timer = setTimeout(() => {
      void fetchBalance();
    }, 0);
    return () => clearTimeout(timer);
  }, [fetchBalance]);

  return {
    balance: tradingAvailable,
    totalBalance: tradingTotal,
    loading,
    walletAddress,
    refetch: async () => {
      setLoading(true);
      await fetchBalance();
      setLoading(false);
    },
    syncAfterMutation: fetchBalance,
  };
};

function formatUnits(raw: string): string {
  const parsed = Number(raw || "0");
  if (!Number.isFinite(parsed)) return "0.00";
  return (parsed / 100).toFixed(2);
}
