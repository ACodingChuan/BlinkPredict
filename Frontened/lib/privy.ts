type WalletLike = {
  type?: string;
  address?: string;
  chainType?: string;
  chain_type?: string;
};

type PrivyUserLike = {
  wallet?: WalletLike;
  linkedAccounts?: WalletLike[];
};

const isSolanaChain = (value?: string) => typeof value === "string" && value.toLowerCase() === "solana";

const isLikelySolanaAddress = (value?: string) =>
  typeof value === "string" && value.length >= 32 && !value.startsWith("0x");

export const getSolanaWalletAddress = (user?: PrivyUserLike | null): string | null => {
  if (!user) return null;

  const primary = user.wallet;
  if (primary?.address && (isSolanaChain(primary.chainType) || isSolanaChain(primary.chain_type))) {
    return primary.address;
  }

  const linked = Array.isArray(user.linkedAccounts) ? user.linkedAccounts : [];
  for (const account of linked) {
    if (account?.type !== "wallet" || !account.address) continue;
    if (isSolanaChain(account.chainType) || isSolanaChain(account.chain_type)) {
      return account.address;
    }
  }

  // Some Privy payloads don't include chainType fields on the active Solana wallet.
  if (primary?.address && isLikelySolanaAddress(primary.address)) {
    return primary.address;
  }
  return null;
};
