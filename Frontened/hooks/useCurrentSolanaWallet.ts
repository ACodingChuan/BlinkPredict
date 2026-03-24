import { useMemo } from "react";
import { usePrivy } from "@privy-io/react-auth";
import { useWallets } from "@privy-io/react-auth/solana";
import { getSolanaWalletAddress } from "@/lib/privy";

type PrivyWalletLike = {
  address?: string;
  chainType?: string;
  chain_type?: string;
};

type PrivyUserLike = {
  wallet?: PrivyWalletLike;
  linkedAccounts?: Array<{
    type?: string;
    address?: string;
    chainType?: string;
    chain_type?: string;
  }>;
};

export function useCurrentSolanaWallet() {
  const { user } = usePrivy();
  const { wallets } = useWallets();

  const walletAddress = useMemo(
    () =>
      getSolanaWalletAddress(user as PrivyUserLike | null) ||
      wallets.find((wallet) => typeof wallet.address === "string" && wallet.address.length > 0)?.address ||
      null,
    [user, wallets],
  );

  const wallet = useMemo(() => {
    if (!walletAddress) {
      return wallets[0] || null;
    }
    return wallets.find((item) => item.address === walletAddress) || wallets[0] || null;
  }, [walletAddress, wallets]);

  return {
    walletAddress,
    wallet,
    wallets,
  };
}
