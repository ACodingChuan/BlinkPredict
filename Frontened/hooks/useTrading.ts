import { Buffer } from "buffer";
import { useState } from "react";
import api from "@/app/utils/axiosInstance";
import { Market, PlaceOrderCommandResponse, TransactionEnvelope } from "@/types/market";
import { toast } from "sonner";
import { usePrivy } from "@privy-io/react-auth";
import { useSignAndSendTransaction, useSignMessage } from "@privy-io/react-auth/solana";
import { useUSDCStore } from "@/store/usdcStore";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import {
  buildOrderIntent,
  buildOrderSignatureMessage,
  normalizeOrderParams,
  type BuildOrderIntentParams,
  type NormalizedOrderParams,
} from "@/lib/order-signature";
import { PublicKey } from "@solana/web3.js";

interface TradeParams {
  market: Market;
  action: "buy" | "sell";
  outcome: "yes" | "no";
  orderType: "market" | "limit" | "split" | "merge" | "claim";
  amount: string;
  limitPrice: number;
  expireTime?: string;
  onAccepted?: (payload: PlaceOrderCommandResponse) => void;
}

export const useTrading = () => {
  const [loading, setLoading] = useState(false);
  const { getAccessToken } = usePrivy();
  const { signAndSendTransaction } = useSignAndSendTransaction();
  const { signMessage } = useSignMessage();
  const { wallet, walletAddress } = useCurrentSolanaWallet();
  const syncBalance = useUSDCStore((state) => state.syncBalance);

  const triggerBalanceSync = () => {
    if (walletAddress) {
      void syncBalance(walletAddress);
    }
  };

  const maybeSendTransaction = async (payload: TransactionEnvelope) => {
    if (!payload.tx_message) {
      toast.message(payload.message);
      return true;
    }

    if (!wallet) {
      toast.error("No wallet connected");
      return false;
    }

    const raw = Buffer.from(payload.tx_message, "base64");
    await signAndSendTransaction({
      transaction: new Uint8Array(raw),
      wallet,
      chain: "solana:devnet",
    });
    toast.success(payload.message || "Transaction submitted");
    return true;
  };

  const getAuthToken = async (): Promise<string | null> => {
    const token = await getAccessToken();
    if (!token) {
      toast.error("Authentication required");
      return null;
    }
    return token;
  };

  const signOrderMessage = async (message: Uint8Array) => {
    if (!wallet) {
      toast.error("No wallet available for signing");
      return null;
    }

    try {
      const signedOutput = await wallet.signMessage({
        message,
      });
      return Buffer.from(signedOutput.signature).toString("base64");
    } catch (walletError) {
      try {
        const signedOutput = await signMessage({
          message,
          wallet,
        });
        return Buffer.from(signedOutput.signature).toString("base64");
      } catch (hookError) {
        console.error("wallet.signMessage failed", walletError);
        console.error("privy signMessage failed", hookError);
        throw hookError;
      }
    }
  };

  const placeOrder = async ({ market, action, outcome, orderType, amount, limitPrice, expireTime, onAccepted }: TradeParams) => {
    setLoading(true);
    try {
      // 处理 split/merge/claim 操作 (保持不变)
      if (orderType === "split" || orderType === "merge") {
        const token = await getAuthToken();
        if (!token) return false;
        const endpoint = orderType === "split" ? "/orders/split" : "/orders/merge";
        const { data } = await api.post<TransactionEnvelope>(
          endpoint,
          {
            market_id: market.market_id,
            collateral_mint: market.collateral_mint,
            amount: Math.floor(Number(amount || "0") * 1_000_000),
          },
          { headers: { "privy-id-token": token } },
        );
        const ok = await maybeSendTransaction(data);
        if (ok) triggerBalanceSync();
        return ok;
      }

      if (orderType === "claim") {
        const token = await getAuthToken();
        if (!token) return false;
        const { data } = await api.post<TransactionEnvelope>(
          "/claims",
          { market_id: market.market_id, collateral_mint: market.collateral_mint, amount: 0 },
          { headers: { "privy-id-token": token } },
        );
        const ok = await maybeSendTransaction(data);
        if (ok) triggerBalanceSync();
        return ok;
      }

      // 验证输入
      const parsedAmount = Number(amount || "0");
      if (parsedAmount <= 0) {
        toast.error("Amount must be greater than 0");
        return false;
      }

      // 限价单价格验证
      if (orderType === "limit") {
        const maybePriceTick = toPriceTick(limitPrice);
        if (!maybePriceTick) {
          toast.error("Limit price must be between 0.01 and 0.99");
          return false;
        }
        if (!expireTime) {
          toast.error("Limit order expire time is required");
          return false;
        }
      }

      // 获取认证 token
      const token = await getAuthToken();
      if (!token) return false;

      // 获取钱包地址
      if (!walletAddress) {
        toast.error("No Solana wallet connected");
        return false;
      }

      // 归一化订单参数 (No -> Yes 转换)
      const normalizedParams: NormalizedOrderParams = {
        action,
        outcome,
        price: orderType === "limit" ? limitPrice : 0, // 市价单价格由滑点决定，这里传 0
        amount: parsedAmount,
        type: orderType as "limit" | "market",
      };
      const normalized = normalizeOrderParams(normalizedParams);

      // 构建订单意图
      const priceTick = normalized.priceTick;
      const lotsOrAmount = normalized.lotsOrAmount;
      const isMarketBuy = normalized.isMarketBuy;

      // 市价单：买入用 spendAmount，卖出用 qtyLots
      // 限价单：只用 qtyLots，spendAmount = 0
      const qtyLots = isMarketBuy ? 0 : lotsOrAmount;
      const spendAmount = isMarketBuy ? lotsOrAmount : 0;

      // 计算 expireTime (Unix 秒级时间戳)
      let expireTimeUnix = 0;
      if (orderType === "limit" && expireTime) {
        expireTimeUnix = Math.floor(new Date(expireTime).getTime() / 1000);
      }

      // 使用新的 Borsh 签名逻辑
      const buildParams: BuildOrderIntentParams = {
        walletAddress: new PublicKey(walletAddress),
        marketId: market.market_id,
        originalAction: normalized.originalAction,
        originalOutcome: normalized.originalOutcome,
        originalPriceTick: normalized.originalPriceTick,
        side: normalized.side,
        orderType: orderType === "limit" ? "limit" : "market",
        priceTick,
        qtyLots,
        spendAmount,
        expireTime: expireTimeUnix,
      };

      const { intent, messageHash } = buildOrderIntent(buildParams);
      const signableMessage = buildOrderSignatureMessage(messageHash);

      // 调用钱包签名。对外部钱包使用可读文本包裹，避免 Phantom 将原始二进制误判为 transaction payload。
      const signature = await signOrderMessage(signableMessage);
      if (!signature) {
        return false;
      }
      const nonce = intent.nonce.toString();

      // 幂等 key
      const idempotencyKey = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random()}`;

      // 发送 HTTP 请求 (使用新的请求体结构)
      const { data } = await api.post<PlaceOrderCommandResponse & { code?: string }>(
        "/orders",
        {
          market_id: market.market_id,
          wallet_address: walletAddress,
          original_action: normalized.originalAction,
          original_outcome: normalized.originalOutcome,
          original_price_tick: normalized.originalPriceTick,
          side: normalized.side,
          order_type: orderType,
          price_tick: priceTick,
          qty_lots: qtyLots,
          spend_amount: spendAmount,
          expire_time: expireTimeUnix,
          nonce,
          signature,
        },
        { headers: { "privy-id-token": token, "Idempotency-Key": idempotencyKey } },
      );

      if (data.code === "command_bus_not_configured") {
        toast.error("Command bus is not configured");
        return false;
      }

      toast.success("Order command accepted", {
        description: `command_id: ${data.command_id}`,
      });
      onAccepted?.(data);
      triggerBalanceSync();
      return true;
    } catch (error: unknown) {
      const response = typeof error === "object" && error !== null && "response" in error ? (error as { response?: { data?: { message?: string; code?: string } } }).response : undefined;
      const message = response?.data?.message || (error instanceof Error ? error.message : "Request failed");
      if (response?.data?.code === "command_bus_not_configured") {
        toast.error("Command bus is not configured", { description: message });
        return false;
      }
      toast.error(message);
      return false;
    } finally {
      setLoading(false);
    }
  };

  return { placeOrder, loading };
};
function toPriceTick(price: number): number | undefined {
  if (!Number.isFinite(price)) {
    return undefined;
  }
  const tick = Math.round(price * 100);
  if (tick < 1 || tick > 99) {
    return undefined;
  }
  return tick;
}
