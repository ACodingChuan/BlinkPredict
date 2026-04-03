"use client";

import { Buffer } from "buffer";
import { sha256 } from "@noble/hashes/sha2.js";
import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { useConnection } from "@solana/wallet-adapter-react";
import { useWalletModal } from "@solana/wallet-adapter-react-ui";
import { PublicKey, SystemProgram, Transaction, TransactionInstruction } from "@solana/web3.js";

import api from "@/app/utils/axiosInstance";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
import { buildOrderIntent, encodeAmountToUnits, encodePriceToTick } from "@/lib/order-signature";
import { generateClientSnowflake } from "@/lib/snowflake";
import { useUSDCStore } from "@/store/usdcStore";
import type {
  Market,
  MarketsResponse,
  OpenOrdersResponse,
  OrderbookSnapshot,
  PlaceOrderCommandResponse,
  TradesResponse,
} from "@/types/market";

type WalletAccountResponse = {
  wallet_address: string;
  collateral_total_units: string;
  collateral_free_units: string;
  collateral_locked_units: string;
  collateral_pending_units: string;
};

type PositionResponse = {
  wallet_address: string;
  yes_free_lots: string;
  yes_locked_lots: string;
  yes_pending_lots: string;
  no_free_lots: string;
  no_locked_lots: string;
  no_pending_lots: string;
  collateral_locked_units: string;
};

type OrderFormState = {
  action: "buy" | "sell";
  outcome: "yes" | "no";
  orderType: "limit" | "market";
  amount: string;
  limitPrice: string;
  expiryTs: string;
};

const defaultForm: OrderFormState = {
  action: "buy",
  outcome: "yes",
  orderType: "limit",
  amount: "1.00",
  limitPrice: "0.55",
  expiryTs: defaultExpiry(),
};

const TOKEN_PROGRAM_ID = new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
const TOKEN_2022_PROGRAM_ID = new PublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
const ASSOCIATED_TOKEN_PROGRAM_ID = new PublicKey("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");

export default function DevTradePage() {
  const { connection } = useConnection();
  const { wallet, walletAddress } = useCurrentSolanaWallet();
  const { setVisible } = useWalletModal();
  const walletVUSDC = useUSDCStore((state) => state.balance);
  const syncWalletVUSDC = useUSDCStore((state) => state.syncBalance);

  const [markets, setMarkets] = useState<Market[]>([]);
  const [selectedMarketId, setSelectedMarketId] = useState<string>("");
  const [orderbook, setOrderbook] = useState<OrderbookSnapshot | null>(null);
  const [trades, setTrades] = useState<TradesResponse["trades"]>([]);
  const [openOrders, setOpenOrders] = useState<OpenOrdersResponse["orders"]>([]);
  const [walletAccount, setWalletAccount] = useState<WalletAccountResponse | null>(null);
  const [position, setPosition] = useState<PositionResponse | null>(null);
  const [form, setForm] = useState<OrderFormState>(defaultForm);
  const [depositAmount, setDepositAmount] = useState<string>("10.00");
  const [depositSignature, setDepositSignature] = useState<string>("");
  const [busy, setBusy] = useState<null | "markets" | "faucet" | "deposit" | "order" | "refresh">(null);
  const [lastAccepted, setLastAccepted] = useState<PlaceOrderCommandResponse | null>(null);
  const [lastError, setLastError] = useState<string>("");

  const selectedMarket = useMemo(
    () => markets.find((market) => market.market_id === selectedMarketId) ?? null,
    [markets, selectedMarketId],
  );

  useEffect(() => {
    void loadMarkets();
  }, []);

  useEffect(() => {
    if (!selectedMarketId && markets.length > 0) {
      setSelectedMarketId(markets[0].market_id);
    }
  }, [markets, selectedMarketId]);

  useEffect(() => {
    if (!selectedMarketId) return;
    void refreshMarketData(selectedMarketId);
  }, [selectedMarketId]);

  useEffect(() => {
    if (!walletAddress) {
      setWalletAccount(null);
      return;
    }
    void refreshWalletAccount(walletAddress);
  }, [walletAddress]);

  useEffect(() => {
    if (!selectedMarketId || !walletAddress) {
      setPosition(null);
      setOpenOrders([]);
      return;
    }
    void refreshWalletMarketData(selectedMarketId, walletAddress);
  }, [selectedMarketId, walletAddress]);

  useEffect(() => {
    if (!walletAddress) return;
    void syncWalletVUSDC(walletAddress);
  }, [walletAddress, syncWalletVUSDC]);

  async function loadMarkets() {
    try {
      setBusy("markets");
      const { data } = await api.get<MarketsResponse>("/markets");
      setMarkets(data.markets ?? []);
    } catch (error) {
      const message = toErrorMessage(error, "Failed to load markets");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function refreshMarketData(marketId: string) {
    try {
      setBusy("refresh");
      const [{ data: nextBook }, { data: nextTrades }] = await Promise.all([
        api.get<OrderbookSnapshot>(`/orderbook/${marketId}`),
        api.get<TradesResponse>(`/trades/${marketId}`),
      ]);
      setOrderbook(nextBook);
      setTrades(nextTrades.trades ?? []);
    } catch (error) {
      const message = toErrorMessage(error, "Failed to refresh market data");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function refreshWalletAccount(address: string) {
    try {
      setBusy("refresh");
      const { data } = await api.get<WalletAccountResponse>("/wallet-account", { params: { wallet_address: address } });
      setWalletAccount(data);
    } catch (error) {
      const message = toErrorMessage(error, "Failed to refresh wallet account");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function refreshWalletMarketData(marketId: string, address: string) {
    try {
      setBusy("refresh");
      const [{ data: nextPosition }, { data: nextOpenOrders }] = await Promise.all([
        api.get<PositionResponse>(`/positions/${marketId}`, { params: { wallet_address: address } }),
        api.get<OpenOrdersResponse>(`/orders/open/${marketId}`, { params: { wallet_address: address } }),
      ]);
      setPosition(nextPosition);
      setOpenOrders(nextOpenOrders.orders ?? []);
    } catch (error) {
      const message = toErrorMessage(error, "Failed to refresh market wallet data");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function handleFaucetClaim() {
    if (!walletAddress) {
      toast.error("Connect wallet first");
      return;
    }
    try {
      setBusy("faucet");
      const { data } = await api.post("/faucet/claim", { wallet_address: walletAddress });
      toast.success("Faucet submitted", {
        description: typeof data?.signature === "string" ? data.signature : "Projection updated",
      });
      await refreshWalletAccount(walletAddress);
      if (selectedMarketId) {
        await refreshWalletMarketData(selectedMarketId, walletAddress);
      }
    } catch (error) {
      const message = toErrorMessage(error, "Faucet claim failed");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function handlePlaceOrder() {
    if (!walletAddress || !wallet?.signMessage) {
      toast.error("Wallet connection with signMessage support is required");
      return;
    }
    if (!selectedMarket) {
      toast.error("Select a market first");
      return;
    }

    const parsedAmount = Number(form.amount);
    if (!Number.isFinite(parsedAmount) || parsedAmount <= 0) {
      toast.error("Amount must be greater than 0");
      return;
    }

    const programId = process.env.NEXT_PUBLIC_PROGRAM_ID;
    if (!programId) {
      toast.error("Missing NEXT_PUBLIC_PROGRAM_ID");
      return;
    }

    let limitPriceTick = 50;
    if (form.orderType === "limit") {
      const parsedPrice = Number(form.limitPrice);
      if (!Number.isFinite(parsedPrice) || parsedPrice <= 0 || parsedPrice >= 1) {
        toast.error("Limit price must be between 0.01 and 0.99");
        return;
      }
      limitPriceTick = encodePriceToTick(parsedPrice);
    } else if (orderbook) {
      limitPriceTick = deriveProtectionTick(orderbook, form.action, form.outcome);
    }

    const expiryTs =
      form.orderType === "limit" && form.expiryTs
        ? Math.floor(new Date(form.expiryTs).getTime() / 1000)
        : 0;

    try {
      setBusy("order");
      const { intent } = buildOrderIntent({
        programId: new PublicKey(programId),
        market: new PublicKey(selectedMarket.market_pda),
        user: new PublicKey(walletAddress),
        side: form.action,
        outcome: form.outcome,
        orderType: form.orderType,
        limitPrice: limitPriceTick,
        totalAmount: encodeAmountToUnits(parsedAmount),
        expiryTs,
      });

      const signed = await wallet.signMessage(buildOrderIntent({
        programId: new PublicKey(programId),
        market: new PublicKey(selectedMarket.market_pda),
        user: new PublicKey(walletAddress),
        side: form.action,
        outcome: form.outcome,
        orderType: form.orderType,
        limitPrice: limitPriceTick,
        totalAmount: encodeAmountToUnits(parsedAmount),
        expiryTs,
        nonce: intent.nonce,
      }).signableMessage);

      const signature = Buffer.from(signed).toString("base64");
      const traceId = generateClientSnowflake();
      const idempotencyKey = generateClientSnowflake();

      const { data } = await api.post<PlaceOrderCommandResponse>(
        "/orders",
        {
          version: intent.version,
          program_id: programId,
          market: selectedMarket.market_pda,
          user: walletAddress,
          side: form.action,
          outcome: form.outcome,
          order_type: form.orderType,
          limit_price: limitPriceTick,
          total_amount: encodeAmountToUnits(parsedAmount),
          expiry_ts: expiryTs,
          nonce: intent.nonce.toString(),
          signature,
        },
        {
          headers: {
            "Idempotency-Key": idempotencyKey,
            "X-Trace-Id": traceId,
          },
        },
      );

      setLastAccepted(data);
      toast.success("Order command accepted", { description: data.command_id });
      await refreshMarketData(selectedMarket.market_id);
      await refreshWalletAccount(walletAddress);
      await refreshWalletMarketData(selectedMarket.market_id, walletAddress);
    } catch (error) {
      const message = toErrorMessage(error, "Place order failed");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  async function handleDeposit() {
    if (!wallet.publicKey || !wallet.sendTransaction || !walletAddress) {
      toast.error("Connect wallet first");
      return;
    }
    const programIDRaw = process.env.NEXT_PUBLIC_PROGRAM_ID;
    const vusdcMintRaw = process.env.NEXT_PUBLIC_VUSDC_MINT;
    const globalVaultRaw = process.env.NEXT_PUBLIC_GLOBAL_VAULT;
    if (!programIDRaw || !vusdcMintRaw || !globalVaultRaw) {
      toast.error("Missing NEXT_PUBLIC_PROGRAM_ID / NEXT_PUBLIC_VUSDC_MINT / NEXT_PUBLIC_GLOBAL_VAULT");
      return;
    }
    const parsedAmount = Number(depositAmount);
    if (!Number.isFinite(parsedAmount) || parsedAmount <= 0) {
      toast.error("Deposit amount must be greater than 0");
      return;
    }

    try {
      setBusy("deposit");
      const programID = new PublicKey(programIDRaw);
      const vusdcMint = new PublicKey(vusdcMintRaw);
      const globalVault = new PublicKey(globalVaultRaw);
      const user = wallet.publicKey;
      const amountUnits = encodeAmountToUnits(parsedAmount);

      const mintAccountInfo = await connection.getAccountInfo(vusdcMint);
      const tokenProgramID = mintAccountInfo?.owner?.equals(TOKEN_2022_PROGRAM_ID) ? TOKEN_2022_PROGRAM_ID : TOKEN_PROGRAM_ID;
      const userTokenAccount = deriveAta(user, vusdcMint, tokenProgramID);
      const [configPDA] = PublicKey.findProgramAddressSync([Buffer.from("config")], programID);
      const [userLedgerPDA] = PublicKey.findProgramAddressSync([Buffer.from("user_ledger"), user.toBuffer()], programID);
      const [vaultAuthorityPDA] = PublicKey.findProgramAddressSync([Buffer.from("global_vault_authority")], programID);

      const instruction = new TransactionInstruction({
        programId: programID,
        keys: [
          { pubkey: user, isSigner: true, isWritable: true },
          { pubkey: configPDA, isSigner: false, isWritable: false },
          { pubkey: userLedgerPDA, isSigner: false, isWritable: true },
          { pubkey: userTokenAccount, isSigner: false, isWritable: true },
          { pubkey: globalVault, isSigner: false, isWritable: true },
          { pubkey: vaultAuthorityPDA, isSigner: false, isWritable: false },
          { pubkey: vusdcMint, isSigner: false, isWritable: false },
          { pubkey: tokenProgramID, isSigner: false, isWritable: false },
          { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
        ],
        data: encodeDepositInstruction(amountUnits),
      });

      const tx = new Transaction().add(instruction);
      const signature = await wallet.sendTransaction(tx, connection);
      setDepositSignature(signature);

      await api.post("/deposits", {
        signature,
        wallet_address: walletAddress,
        amount_units: amountUnits,
      });

      toast.success("Deposit transaction submitted", { description: signature });
      await syncWalletVUSDC(walletAddress, [0, 1500, 3500]);
      await refreshWalletAccount(walletAddress);
      if (selectedMarketId) {
        await refreshWalletMarketData(selectedMarketId, walletAddress);
      }
    } catch (error) {
      const message = toErrorMessage(error, "Deposit failed");
      setLastError(message);
      toast.error(message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="min-h-screen bg-[linear-gradient(180deg,#fbfaf7_0%,#f5f3ed_100%)] text-zinc-900">
      <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
        <div className="mb-8 flex flex-wrap items-center justify-between gap-4">
          <div>
            <div className="text-xs font-semibold uppercase tracking-[0.3em] text-emerald-700">Developer Flow</div>
            <h1 className="mt-2 text-3xl font-semibold tracking-tight">Trade Sandbox</h1>
            <p className="mt-2 max-w-2xl text-sm text-zinc-600">
              This page exists only to test the core path: connect wallet, claim faucet, sign order, submit order, inspect wallet/book/trades.
            </p>
          </div>
          <div className="flex items-center gap-3">
            <Link href="/" className="rounded-full border border-zinc-300 px-4 py-2 text-sm font-medium text-zinc-700 hover:bg-white">
              Back Home
            </Link>
            <button
              type="button"
              onClick={() => setVisible(true)}
              className="rounded-full bg-zinc-900 px-4 py-2 text-sm font-semibold text-white"
            >
              {walletAddress ? `${walletAddress.slice(0, 6)}...${walletAddress.slice(-4)}` : "Connect Wallet"}
            </button>
          </div>
        </div>

        <div className="grid gap-6 xl:grid-cols-[360px_1fr]">
          <section className="space-y-6">
            <Panel title="Wallet">
              <div className="space-y-3 text-sm">
                <Field label="Address" value={walletAddress ?? "Not connected"} mono />
                <Field label="API" value={process.env.NEXT_PUBLIC_API_BASE_URL || "http://localhost:8080/api"} mono />
                <div className="grid grid-cols-2 gap-3">
                  <button
                    type="button"
                    onClick={() => void handleFaucetClaim()}
                    disabled={!walletAddress || busy === "faucet"}
                    className="rounded-2xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {busy === "faucet" ? "Claiming..." : "Claim Faucet"}
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      if (walletAddress) {
                        void refreshWalletAccount(walletAddress);
                      }
                      if (selectedMarketId && walletAddress) {
                        void refreshWalletMarketData(selectedMarketId, walletAddress);
                      }
                    }}
                    disabled={!walletAddress || busy === "refresh"}
                    className="rounded-2xl border border-zinc-300 px-4 py-3 text-sm font-semibold text-zinc-700 disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    Refresh Wallet
                  </button>
                </div>
                <DataGrid
                  items={[
                    ["Wallet vUSDC", walletVUSDC],
                    ["Free USDC", formatUnits(walletAccount?.collateral_free_units)],
                    ["Total USDC", formatUnits(walletAccount?.collateral_total_units)],
                    ["Locked USDC", formatUnits(walletAccount?.collateral_locked_units)],
                    ["Pending USDC", formatUnits(walletAccount?.collateral_pending_units)],
                    ["YES Free", formatUnits(position?.yes_free_lots)],
                    ["YES Locked", formatUnits(position?.yes_locked_lots)],
                    ["YES Pending", formatUnits(position?.yes_pending_lots)],
                    ["NO Free", formatUnits(position?.no_free_lots)],
                    ["NO Locked", formatUnits(position?.no_locked_lots)],
                    ["NO Pending", formatUnits(position?.no_pending_lots)],
                  ]}
                />
                <div className="space-y-3 rounded-3xl border border-zinc-200 bg-stone-50 p-4">
                  <div className="text-[11px] font-semibold uppercase tracking-[0.2em] text-zinc-500">Real Deposit</div>
                  <Label text="Amount">
                    <Input value={depositAmount} onChange={setDepositAmount} placeholder="10.00" />
                  </Label>
                  <button
                    type="button"
                    onClick={() => void handleDeposit()}
                    disabled={!walletAddress || busy === "deposit"}
                    className="w-full rounded-2xl bg-sky-700 px-4 py-3 text-sm font-semibold text-white disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {busy === "deposit" ? "Depositing..." : "Deposit To Trading Ledger"}
                  </button>
                  <Field label="Last Deposit Signature" value={depositSignature || "-"} mono />
                </div>
              </div>
            </Panel>

            <Panel title="Order Form">
              <div className="space-y-4">
                <Label text="Market">
                  <select
                    value={selectedMarketId}
                    onChange={(event) => setSelectedMarketId(event.target.value)}
                    className="w-full rounded-2xl border border-zinc-300 bg-white px-3 py-3 text-sm"
                  >
                    {markets.map((market) => (
                      <option key={market.market_id} value={market.market_id}>
                        {market.title} ({market.market_id})
                      </option>
                    ))}
                  </select>
                </Label>
                <div className="grid grid-cols-3 gap-3">
                  <Label text="Action">
                    <Select value={form.action} onChange={(value) => setForm((prev) => ({ ...prev, action: value as "buy" | "sell" }))}>
                      <option value="buy">Buy</option>
                      <option value="sell">Sell</option>
                    </Select>
                  </Label>
                  <Label text="Outcome">
                    <Select value={form.outcome} onChange={(value) => setForm((prev) => ({ ...prev, outcome: value as "yes" | "no" }))}>
                      <option value="yes">Yes</option>
                      <option value="no">No</option>
                    </Select>
                  </Label>
                  <Label text="Type">
                    <Select value={form.orderType} onChange={(value) => setForm((prev) => ({ ...prev, orderType: value as "limit" | "market" }))}>
                      <option value="limit">Limit</option>
                      <option value="market">Market</option>
                    </Select>
                  </Label>
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <Label text="Amount">
                    <Input value={form.amount} onChange={(value) => setForm((prev) => ({ ...prev, amount: value }))} placeholder="1.00" />
                  </Label>
                  <Label text="Limit Price">
                    <Input
                      value={form.limitPrice}
                      onChange={(value) => setForm((prev) => ({ ...prev, limitPrice: value }))}
                      placeholder="0.55"
                      disabled={form.orderType === "market"}
                    />
                  </Label>
                </div>
                <Label text="Expiry">
                  <Input
                    type="datetime-local"
                    value={form.expiryTs}
                    onChange={(value) => setForm((prev) => ({ ...prev, expiryTs: value }))}
                    disabled={form.orderType === "market"}
                  />
                </Label>
                <button
                  type="button"
                  onClick={() => void handlePlaceOrder()}
                  disabled={!walletAddress || !selectedMarket || busy === "order"}
                  className="w-full rounded-2xl bg-zinc-900 px-4 py-3 text-sm font-semibold text-white disabled:cursor-not-allowed disabled:opacity-50"
                >
                  {busy === "order" ? "Signing + Sending..." : "Sign And Place Order"}
                </button>
              </div>
            </Panel>

            <Panel title="Last Result">
              <div className="space-y-2 text-sm">
                <Field label="Command ID" value={lastAccepted?.command_id || "-"} mono />
                <Field label="Order ID" value={lastAccepted?.order_id || "-"} mono />
                <Field label="Idempotency Key" value={lastAccepted?.idempotency_key || "-"} mono />
                <Field label="Last Error" value={lastError || "-"} />
              </div>
            </Panel>
          </section>

          <section className="space-y-6">
            <Panel title="Selected Market">
              <div className="grid gap-4 md:grid-cols-2">
                <Field label="Title" value={selectedMarket?.title || "-"} />
                <Field label="Market ID" value={selectedMarket?.market_id || "-"} mono />
                <Field label="Market PDA" value={selectedMarket?.market_pda || "-"} mono />
                <Field label="Status" value={selectedMarket?.status || "-"} />
              </div>
            </Panel>

            <div className="grid gap-6 xl:grid-cols-2">
              <Panel title="Orderbook">
                <div className="grid grid-cols-2 gap-4 text-sm">
                  <LevelTable title="Bids" rows={orderbook?.bids ?? []} />
                  <LevelTable title="Asks" rows={orderbook?.asks ?? []} />
                </div>
              </Panel>

              <Panel title="Open Orders">
                <SimpleTable
                  headers={["ID", "Side", "Qty", "Status"]}
                  rows={openOrders.map((order) => [order.id, order.side || "-", order.quantity || "-", order.status || "-"])}
                  empty="No open orders"
                />
              </Panel>
            </div>

            <Panel title="Recent Trades">
              <SimpleTable
                headers={["ID", "Price", "Qty", "Executed"]}
                rows={trades.map((trade) => [trade.id, trade.price || "-", trade.quantity || "-", trade.executed_at || "-"])}
                empty="No trades yet"
              />
            </Panel>
          </section>
        </div>
      </div>
    </div>
  );
}

function Panel({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-[1.75rem] border border-black/5 bg-white/90 p-5 shadow-sm">
      <div className="mb-4 text-xs font-semibold uppercase tracking-[0.25em] text-zinc-500">{title}</div>
      {children}
    </div>
  );
}

function Label({ text, children }: { text: string; children: React.ReactNode }) {
  return (
    <label className="block space-y-2">
      <span className="text-xs font-semibold uppercase tracking-[0.2em] text-zinc-500">{text}</span>
      {children}
    </label>
  );
}

function Input({
  value,
  onChange,
  placeholder,
  disabled,
  type = "text",
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  disabled?: boolean;
  type?: string;
}) {
  return (
    <input
      type={type}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      placeholder={placeholder}
      disabled={disabled}
      className="w-full rounded-2xl border border-zinc-300 bg-white px-3 py-3 text-sm disabled:cursor-not-allowed disabled:bg-zinc-100"
    />
  );
}

function Select({
  value,
  onChange,
  children,
}: {
  value: string;
  onChange: (value: string) => void;
  children: React.ReactNode;
}) {
  return (
    <select
      value={value}
      onChange={(event) => onChange(event.target.value)}
      className="w-full rounded-2xl border border-zinc-300 bg-white px-3 py-3 text-sm"
    >
      {children}
    </select>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="rounded-2xl bg-stone-50 px-3 py-3">
      <div className="text-[11px] font-semibold uppercase tracking-[0.2em] text-zinc-500">{label}</div>
      <div className={`mt-2 break-all text-sm text-zinc-900 ${mono ? "font-mono" : ""}`}>{value}</div>
    </div>
  );
}

function DataGrid({ items }: { items: Array<[string, string]> }) {
  return (
    <div className="grid grid-cols-2 gap-3">
      {items.map(([label, value]) => (
        <Field key={label} label={label} value={value} />
      ))}
    </div>
  );
}

function LevelTable({
  title,
  rows,
}: {
  title: string;
  rows: Array<{ price: string; total_volume: string }>;
}) {
  return (
    <div>
      <div className="mb-2 text-xs font-semibold uppercase tracking-[0.2em] text-zinc-500">{title}</div>
      <div className="space-y-2">
        {rows.length === 0 ? <div className="rounded-2xl bg-stone-50 px-3 py-3 text-sm text-zinc-500">Empty</div> : null}
        {rows.slice(0, 8).map((row) => (
          <div key={`${title}-${row.price}`} className="grid grid-cols-2 rounded-2xl bg-stone-50 px-3 py-3 text-sm">
            <span>{formatUnits(row.total_volume)}</span>
            <span className="text-right">{formatPrice(row.price)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function SimpleTable({
  headers,
  rows,
  empty,
}: {
  headers: string[];
  rows: string[][];
  empty: string;
}) {
  return (
    <div className="overflow-hidden rounded-3xl border border-zinc-200">
      <div className="grid bg-stone-100 px-4 py-3 text-[11px] font-semibold uppercase tracking-[0.2em] text-zinc-500" style={{ gridTemplateColumns: `repeat(${headers.length}, minmax(0, 1fr))` }}>
        {headers.map((header) => (
          <div key={header}>{header}</div>
        ))}
      </div>
      {rows.length === 0 ? <div className="px-4 py-6 text-sm text-zinc-500">{empty}</div> : null}
      {rows.map((row, index) => (
        <div key={`${row[0]}-${index}`} className="grid border-t border-zinc-200 px-4 py-3 text-sm" style={{ gridTemplateColumns: `repeat(${headers.length}, minmax(0, 1fr))` }}>
          {row.map((cell, cellIndex) => (
            <div key={`${cellIndex}-${cell}`} className={cellIndex === 0 ? "font-mono break-all" : ""}>
              {cell}
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function formatUnits(value?: string) {
  if (!value) return "-";
  const raw = Number(value);
  if (!Number.isFinite(raw)) return value;
  return (raw / 100).toFixed(2);
}

function formatPrice(value?: string) {
  if (!value) return "-";
  const raw = Number(value);
  if (!Number.isFinite(raw)) return value;
  return (raw / 100).toFixed(2);
}

function deriveProtectionTick(orderbook: OrderbookSnapshot, action: "buy" | "sell", outcome: "yes" | "no") {
  if (outcome === "yes") {
    if (action === "buy") {
      return Number(orderbook.best_ask_price || 50) || 50;
    }
    return Number(orderbook.best_bid_price || 50) || 50;
  }
  if (action === "buy") {
    const bestBid = Number(orderbook.best_bid_price || 50) || 50;
    return Math.max(1, 100 - bestBid);
  }
  const bestAsk = Number(orderbook.best_ask_price || 50) || 50;
  return Math.max(1, 100 - bestAsk);
}

function toErrorMessage(error: unknown, fallback: string) {
  const response =
    typeof error === "object" && error !== null && "response" in error
      ? (error as { response?: { data?: { message?: string } } }).response
      : undefined;
  return response?.data?.message || (error instanceof Error ? error.message : fallback);
}

function defaultExpiry() {
  const date = new Date(Date.now() + 60 * 60 * 1000);
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function deriveAta(owner: PublicKey, mint: PublicKey, tokenProgramId: PublicKey) {
  return PublicKey.findProgramAddressSync(
    [owner.toBuffer(), tokenProgramId.toBuffer(), mint.toBuffer()],
    ASSOCIATED_TOKEN_PROGRAM_ID,
  )[0];
}

function encodeDepositInstruction(amountUnits: number) {
  const data = new Uint8Array(16);
  data.set(anchorDiscriminator("deposit"), 0);
  const view = new DataView(data.buffer);
  view.setBigUint64(8, BigInt(amountUnits), true);
  return Buffer.from(data);
}

function anchorDiscriminator(name: string) {
  return sha256(new TextEncoder().encode(`global:${name}`)).slice(0, 8);
}
