import { usePrivy } from "@privy-io/react-auth";
import { useEffect } from "react";
import { useUSDCStore } from "@/store/usdcStore";

export const useUSDCBalance = () => {
    const { user, ready, authenticated } = usePrivy();
    const { balance, loading, isRefreshing, fetchBalance, startRefreshing } = useUSDCStore();

    useEffect(() => {
        const walletAddress = user?.wallet?.address;
        if (ready && authenticated && walletAddress) {
            // Initial fetch after auth is fully established.
            fetchBalance(walletAddress);

            // Background poll (keep it light; localnet/devnet RPC can be noisy).
            const interval = setInterval(() => {
                if (walletAddress) fetchBalance(walletAddress);
            }, 15000);

            return () => clearInterval(interval);
        }
    }, [authenticated, fetchBalance, ready, user?.wallet?.address]);

    return {
        balance,
        loading,
        isRefreshing,
        refetch: () => user?.wallet?.address && startRefreshing(user.wallet.address)
    };
};
