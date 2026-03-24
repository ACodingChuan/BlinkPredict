import { useEffect, useState } from "react";
import { usePrivy } from "@privy-io/react-auth";
import api from "@/app/utils/axiosInstance";
import { useUSDCStore } from "@/store/usdcStore";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";

type WalletAccountResponse = {
  collateral_total_units: string;
  collateral_free_units: string;
};

export const useUSDCBalance = () => {
  const { walletAddress } = useCurrentSolanaWallet();
  const { authenticated, getAccessToken } = usePrivy();
  const { balance, loading, isRefreshing, fetchBalance, syncBalance } = useUSDCStore();
  const [tradingTotal, setTradingTotal] = useState("0.00");
  const [tradingAvailable, setTradingAvailable] = useState("0.00");

  useEffect(() => {
    const load = async () => {
      if (!walletAddress) return;
      setTradingTotal("0.00");
      setTradingAvailable("0.00");
      if (authenticated) {
        const token = await getAccessToken();
        if (token) {
          try {
            const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
              headers: { "privy-id-token": token },
            });
            setTradingTotal(formatUnits(data.collateral_total_units));
            setTradingAvailable(formatUnits(data.collateral_free_units));
          } catch {
            // Keep trading balances at 0.00 until the system account is initialized.
          }
        }
      }
      await fetchBalance(walletAddress);
    };
    void load();
  }, [authenticated, fetchBalance, getAccessToken, walletAddress]);

  return {
    balance: tradingAvailable,
    totalBalance: tradingTotal,
    walletBalance: balance,
    loading,
    isRefreshing,
    walletAddress,
    refetch: async () => {
      if (!walletAddress) return;
      await fetchBalance(walletAddress);
    },
    syncAfterMutation: async () => {
      if (!walletAddress) return;
      await syncBalance(walletAddress);
      if (!authenticated) return;
      const token = await getAccessToken();
      if (!token) return;
      try {
        const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
          headers: { "privy-id-token": token },
        });
        setTradingTotal(formatUnits(data.collateral_total_units));
        setTradingAvailable(formatUnits(data.collateral_free_units));
      } catch {
        // noop
      }
    }
  };
};

function formatUnits(raw: string): string {
  const parsed = Number(raw || "0");
  if (!Number.isFinite(parsed)) return "0.00";
  return (parsed / 100).toFixed(2);
}
