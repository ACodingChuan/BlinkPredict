"use client";

import { Buffer } from "buffer";
import { useState } from "react";
import { useConnection } from "@solana/wallet-adapter-react";
import api from "@/app/utils/axiosInstance";
import { encodeAmountToUnits } from "@/lib/order-signature";
import { usePrivy } from "@/lib/auth-client";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import { useSignAndSendTransaction } from "@/hooks/useWalletTransactions";
import { useUSDCBalance } from "@/hooks/useUSDCBalance";
import { useUSDCStore } from "@/store/usdcStore";
import { TransactionEnvelope } from "@/types/market";
import { toast } from "sonner";

export type TradingTransferKind = "deposit" | "withdraw";

const TRADING_SYNC_DELAYS_MS = [0, 1200, 3000, 6000];
const WALLET_SYNC_DELAYS_MS = [0, 1200, 3000, 6000];

type SubmitResponse = {
  status: string;
  signature: string;
  wallet_address: string;
  amount_units: number;
};

export function useTradingTransfers() {
  const [busyKind, setBusyKind] = useState<TradingTransferKind | null>(null);
  const { connection } = useConnection();
  const { getAccessToken, authenticated } = usePrivy();
  const { walletAddress } = useCurrentSolanaWallet();
  const { signAndSendTransaction } = useSignAndSendTransaction();
  const { syncAfterMutation } = useUSDCBalance({ autoFetch: false });
  const syncWalletBalance = useUSDCStore((state) => state.syncBalance);

  const submitTransfer = async (kind: TradingTransferKind, amountText: string) => {
    if (!authenticated) {
      toast.error("Authentication required");
      return false;
    }
    if (!walletAddress) {
      toast.error("No Solana wallet connected");
      return false;
    }

    const parsedAmount = Number(amountText || "0");
    if (!Number.isFinite(parsedAmount) || parsedAmount <= 0) {
      toast.error("Amount must be greater than 0");
      return false;
    }

    const amountUnits = encodeAmountToUnits(parsedAmount);
    if (amountUnits <= 0) {
      toast.error("Amount must be greater than 0");
      return false;
    }

    const token = await getAccessToken();
    if (!token) {
      toast.error("Authentication required");
      return false;
    }

    setBusyKind(kind);
    try {
      const envelopePath = kind === "deposit" ? "/deposits/envelope" : "/withdrawals/envelope";
      const submitPath = kind === "deposit" ? "/deposits" : "/withdrawals";

      const { data: envelope } = await api.post<TransactionEnvelope>(
        envelopePath,
        {
          wallet_address: walletAddress,
          amount_units: amountUnits,
        },
        { headers: { Authorization: `Bearer ${token}` } },
      );

      if (!envelope.tx_message) {
        toast.error(envelope.message || "Transaction envelope is empty");
        return false;
      }

      const raw = Buffer.from(envelope.tx_message, "base64");
      const { signature } = await signAndSendTransaction({
        transaction: new Uint8Array(raw),
      });

      await api.post<SubmitResponse>(
        submitPath,
        {
          signature,
          wallet_address: walletAddress,
          amount_units: amountUnits,
        },
        { headers: { Authorization: `Bearer ${token}` } },
      );

      toast.success(kind === "deposit" ? "Deposit submitted" : "Withdraw submitted", {
        description: signature,
      });
      void syncAfterMutation(TRADING_SYNC_DELAYS_MS);
      void syncWalletBalance(walletAddress, WALLET_SYNC_DELAYS_MS);
      void connection
        .confirmTransaction(signature, "confirmed")
        .then((result) => {
          if (result.value.err) {
            return;
          }
          return syncWalletBalance(walletAddress, [0, 500, 1200]);
        })
        .catch(() => undefined);
      return true;
    } catch (error: unknown) {
      const response =
        typeof error === "object" && error !== null && "response" in error
          ? (error as { response?: { data?: { message?: string } } }).response
          : undefined;
      const message = response?.data?.message || (error instanceof Error ? error.message : "Transfer failed");
      toast.error(message);
      return false;
    } finally {
      setBusyKind(null);
    }
  };

  return {
    busyKind,
    loading: busyKind !== null,
    submitTransfer,
  };
}
