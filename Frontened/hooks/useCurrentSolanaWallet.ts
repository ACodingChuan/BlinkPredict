import { useMemo } from "react";
import { useWallet } from "@solana/wallet-adapter-react";

export function useCurrentSolanaWallet() {
  const wallet = useWallet();

  return useMemo(
    () => ({
      walletAddress: wallet.publicKey?.toBase58() ?? null,
      wallet,
      wallets: wallet.wallets,
    }),
    [wallet],
  );
}
