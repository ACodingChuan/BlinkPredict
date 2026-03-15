import { create } from 'zustand';
import { Connection, PublicKey } from "@solana/web3.js";
import { getAssociatedTokenAddress, getAccount, getMint, TokenAccountNotFoundError } from "@solana/spl-token";

interface USDCState {
    balance: string;
    loading: boolean;
    isRefreshing: boolean; // For animation
    decimals: number;
    decimalsLoaded: boolean;
    fetchBalance: (walletAddress: string) => Promise<void>;
    startRefreshing: (walletAddress: string) => void;
}

const DEFAULT_DECIMALS = 6;

export const useUSDCStore = create<USDCState>((set, get) => ({
    balance: "0.00",
    loading: false,
    isRefreshing: false,
    decimals: DEFAULT_DECIMALS,
    decimalsLoaded: false,

    fetchBalance: async (walletAddress: string) => {
        if (!walletAddress) return;

        try {
            // Only set global loading on first fetch if we want to show a spinner, 
            // but for balance updates usually silent is better or specific loading state.
            // We'll keep loading for initial load.
            // set({ loading: true }); 

            const connection = new Connection(process.env.NEXT_PUBLIC_RPC_URL || 'https://api.devnet.solana.com');
            const walletPublicKey = new PublicKey(walletAddress);
            const vusdcMint = process.env.NEXT_PUBLIC_VUSDC_MINT || "";
            if (!vusdcMint) {
                set({ balance: "0.00" });
                return;
            }
            const usdcMintKey = new PublicKey(vusdcMint);

            const associatedTokenAddress = await getAssociatedTokenAddress(
                usdcMintKey,
                walletPublicKey
            );

            let decimals = get().decimals;
            if (!Number.isFinite(decimals) || decimals < 0) {
                decimals = DEFAULT_DECIMALS;
            }
            if (!get().decimalsLoaded) {
                try {
                    const mint = await getMint(connection, usdcMintKey);
                    decimals = mint.decimals;
                    set({ decimals, decimalsLoaded: true });
                } catch {
                    // keep fallback decimals; don't mark loaded so we can retry later.
                }
            }

            try {
                const account = await getAccount(connection, associatedTokenAddress);
                const bal = Number(account.amount) / Math.pow(10, decimals);
                set({ balance: bal.toFixed(2) });
            } catch (error: unknown) {
                const tokenError = error instanceof TokenAccountNotFoundError || (typeof error === "object" && error !== null && "name" in error && (error as { name?: string }).name === "TokenAccountNotFoundError");
                if (tokenError) {
                    set({ balance: "0.00" });
                } else {
                    console.error("Error fetching token account:", error);
                }
            }
        } catch (error) {
            console.error("Error fetching vUSDC balance:", error);
            set({ balance: "0.00" });
        } finally {
            set({ loading: false });
        }
    },

    startRefreshing: (walletAddress: string) => {
        set({ isRefreshing: true });
        const { fetchBalance } = get();

        // Immediate fetch
        fetchBalance(walletAddress);

        // Poll at 2, 4, 6, 8, 10 seconds
        const delays = [2000, 4000, 6000, 8000, 10000];

        delays.forEach((delay, index) => {
            setTimeout(() => {
                fetchBalance(walletAddress);
                // Turn off refreshing animation after the last poll
                if (index === delays.length - 1) {
                    set({ isRefreshing: false });
                }
            }, delay);
        });
    }
}));
