import { usePrivy } from "@privy-io/react-auth";
import { useEffect } from "react";
import { useUSDCStore } from "@/store/usdcStore";
import { getSolanaWalletAddress } from "@/lib/privy";

export const useUSDCBalance = () => {
    const { user, ready, authenticated } = usePrivy();
    const { balance, loading, isRefreshing, fetchBalance, syncBalance } = useUSDCStore();
    const walletAddress = getSolanaWalletAddress(user as { wallet?: { address?: string; chainType?: string; chain_type?: string }; linkedAccounts?: { type?: string; address?: string; chainType?: string; chain_type?: string }[] } | null);

    useEffect(() => {
        if (ready && authenticated && walletAddress) {
            fetchBalance(walletAddress);
        }
    }, [authenticated, fetchBalance, ready, walletAddress]);

    return {
        balance,
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
        }
    };
};
