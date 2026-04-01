import { Buffer } from "buffer";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import { Transaction, VersionedTransaction } from "@solana/web3.js";

export function useSignAndSendTransaction() {
  const { connection } = useConnection();
  const wallet = useWallet();

  return {
    signAndSendTransaction: async ({ transaction }: { transaction: Uint8Array; wallet?: unknown; chain?: string }) => {
      const decoded = deserializeAnyTransaction(transaction);
      const signature = await wallet.sendTransaction(decoded, connection);
      return { signature };
    },
  };
}

function deserializeAnyTransaction(raw: Uint8Array) {
  try {
    return VersionedTransaction.deserialize(raw);
  } catch {
    return Transaction.from(Buffer.from(raw));
  }
}
