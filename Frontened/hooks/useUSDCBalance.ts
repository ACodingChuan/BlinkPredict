import { useEffect, useState } from "react";
import { usePrivy } from "@privy-io/react-auth";
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

  useEffect(() => {
    const load = async () => {
      if (!walletAddress) return;
      setTradingTotal("0.00");
      setTradingAvailable("0.00");

      // 只从 API 获取交易账户余额
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
    };
    void load();
  }, [authenticated, getAccessToken, walletAddress]);

  return {
    balance: tradingAvailable,
    totalBalance: tradingTotal,
    loading,
    walletAddress,
    refetch: async () => {
      if (!walletAddress || !authenticated) return;
      setLoading(true);
      const token = await getAccessToken();
      if (!token) {
        setLoading(false);
        return;
      }
      try {
        const { data } = await api.get<WalletAccountResponse>("/wallet-account", {
          headers: { "privy-id-token": token },
        });
        setTradingTotal(formatUnits(data.collateral_total_units));
        setTradingAvailable(formatUnits(data.collateral_free_units));
      } catch {
        // noop
      } finally {
        setLoading(false);
      }
    },
    syncAfterMutation: async () => {
      if (!walletAddress || !authenticated) return;
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
