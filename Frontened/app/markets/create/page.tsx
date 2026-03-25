"use client";

import { Buffer } from "buffer";
import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { useSignAndSendTransaction } from "@privy-io/react-auth/solana";
import { Connection, PublicKey, SystemProgram, Transaction, TransactionInstruction } from "@solana/web3.js";
import { ASSOCIATED_TOKEN_PROGRAM_ID, TOKEN_2022_PROGRAM_ID } from "@solana/spl-token";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";
console.log("RPC", process.env.NEXT_PUBLIC_RPC_URL, "MINT", process.env.NEXT_PUBLIC_VUSDC_MINT);

type HermesLatestPrice = {
  id?: string;
  price?: {
    price?: string;
    conf?: string;
    expo?: number;
    publish_time?: number;
  };
  ema_price?: {
    price?: string;
    conf?: string;
    expo?: number;
    publish_time?: number;
  };
};

type FeedCheckState =
  | { status: "idle" }
  | { status: "checking" }
  | {
      status: "valid";
      id: string;
      priceDecimal: string;
      priceInt: string;
      expo: number;
      publishTime: number;
    }
  | { status: "invalid"; reason: string };

export default function CreateMarketPage() {
  const router = useRouter();
  const { wallet, walletAddress } = useCurrentSolanaWallet();
  const { signAndSendTransaction } = useSignAndSendTransaction();
  const localWalletMode = process.env.NEXT_PUBLIC_LOCAL_WALLET_MODE === "true";
  const [loading, setLoading] = useState(false);
  const [successSig, setSuccessSig] = useState("");
  const [copied, setCopied] = useState(false);
  const [mode, setMode] = useState<"creator" | "pyth">("creator");
  const [form, setForm] = useState({
    title: "",
    description: "",
    close_time: "",
    settle_time: "",
    // Pyth (pull) uses a 0x-prefixed 32-byte feed id from the Pyth UI/Hermes world.
    oracle_feed_id: "",
    oracle_condition: "gte" as "gte" | "gt" | "lt" | "lte",
    // Human input (e.g. 250.00). We convert to micro (1e6) before sending.
    oracle_target_price: "250.00",
  });
  const [imageDataUrl, setImageDataUrl] = useState<string>("");
  const [feedCheck, setFeedCheck] = useState<FeedCheckState>({ status: "idle" });
  const [error, setError] = useState("");

  const resolutionAuthority =
    walletAddress || (localWalletMode ? getLocalWalletAddress() : "") || "";

  const targetPriceIntAtExpo = useMemo(() => {
    if (mode !== "pyth") return "";
    if (feedCheck.status !== "valid") return "";
    return decimalToPythPriceInt(form.oracle_target_price, feedCheck.expo);
  }, [feedCheck, form.oracle_target_price, mode]);

  useEffect(() => {
    if (mode !== "pyth") {
      setFeedCheck({ status: "idle" });
      return;
    }

    const raw = form.oracle_feed_id.trim();
    if (!raw) {
      setFeedCheck({ status: "idle" });
      return;
    }

    const value = raw.toLowerCase();
    if (!/^0x[0-9a-f]{64}$/.test(value)) {
      setFeedCheck({ status: "invalid", reason: "Feed id must be 0x + 64 hex chars." });
      return;
    }

    let cancelled = false;
    setFeedCheck({ status: "checking" });

    const timer = setTimeout(() => {
      void (async () => {
        try {
          const url = `https://hermes.pyth.network/api/latest_price_feeds?ids[]=${encodeURIComponent(value)}`;
          const response = await fetch(url, { method: "GET" });
          if (!response.ok) {
            throw new Error(`Hermes request failed (${response.status})`);
          }
          const json = (await response.json()) as HermesLatestPrice[];
          const item = Array.isArray(json) ? json[0] : undefined;
          const id = item?.id?.toLowerCase();
          const expo = item?.price?.expo;
          const publishTime = item?.price?.publish_time;
          const priceInt = item?.price?.price;

          // Hermes returns the feed id without 0x prefix, even if we query with 0x.
          if (!id || normalizeFeedId(id) !== normalizeFeedId(value)) {
            throw new Error("Feed id not found on Hermes.");
          }
          if (typeof expo !== "number" || typeof publishTime !== "number" || !priceInt) {
            throw new Error("Hermes returned an incomplete price payload.");
          }

          const priceDecimal = formatPythPrice(priceInt, expo);

          if (cancelled) return;
          setFeedCheck({
            status: "valid",
            id: value,
            priceDecimal,
            priceInt,
            expo,
            publishTime,
          });
        } catch (err: unknown) {
          if (cancelled) return;
          const message = err instanceof Error ? err.message : "Failed to validate feed id";
          setFeedCheck({ status: "invalid", reason: message });
        }
      })();
    }, 350);

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [form.oracle_feed_id, mode]);

  useEffect(() => {
    if (mode !== "pyth") return;
    if (feedCheck.status !== "valid") return;
    if (!form.title.trim()) return;
    if (!form.close_time || !form.settle_time) return;
    if (!targetPriceIntAtExpo) return;

    const closeISO = new Date(form.close_time).toISOString();
    const settleISO = new Date(form.settle_time).toISOString();
    const conditionSymbol =
      form.oracle_condition === "gt"
        ? ">"
        : form.oracle_condition === "gte"
          ? ">="
          : form.oracle_condition === "lt"
            ? "<"
            : "<=";

    const generated = buildPythDescription({
      title: form.title.trim(),
      closeTimeISO: closeISO,
      earliestSettleTimeISO: settleISO,
      feedId: form.oracle_feed_id.trim().toLowerCase(),
      oracleConditionSymbol: conditionSymbol,
      targetPriceDecimal: form.oracle_target_price.trim(),
    });

    if (generated !== form.description) {
      setForm((current) => ({ ...current, description: generated }));
    }
  }, [
    feedCheck,
    form.close_time,
    form.description,
    form.oracle_condition,
    form.oracle_feed_id,
    form.oracle_target_price,
    form.settle_time,
    form.title,
    mode,
    targetPriceIntAtExpo,
  ]);

  const pythIsValid = mode !== "pyth" ? true : feedCheck.status === "valid";

  if (successSig) {
    return (
      <div className="min-h-screen bg-stone-100 px-4 py-10 dark:bg-zinc-950 sm:px-6 lg:px-8">
        <div className="mx-auto max-w-3xl rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
          <div className="flex items-center gap-3">
            <div className="flex h-10 w-10 items-center justify-center rounded-full bg-emerald-100 dark:bg-emerald-900/40">
              <svg className="h-5 w-5 text-emerald-600 dark:text-emerald-300" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M5 13l4 4L19 7" />
              </svg>
            </div>
            <h1 className="text-2xl font-semibold text-zinc-950 dark:text-zinc-50">Market created</h1>
          </div>
          <p className="mt-4 text-sm text-zinc-500 dark:text-zinc-400">
            Transaction confirmed on-chain. Copy the signature below for your records.
          </p>
          <div className="mt-6">
            <label className="text-xs font-medium uppercase tracking-widest text-zinc-500 dark:text-zinc-400">
              Transaction signature
            </label>
            <div className="mt-2 flex items-center gap-2 rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 dark:border-zinc-700 dark:bg-zinc-950">
              <span className="flex-1 break-all font-mono text-sm text-zinc-900 dark:text-zinc-100">
                {successSig}
              </span>
              <button
                type="button"
                className="shrink-0 rounded-full bg-zinc-900 px-3 py-1.5 text-xs font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900"
                onClick={() => {
                  void navigator.clipboard.writeText(successSig);
                  setCopied(true);
                  setTimeout(() => setCopied(false), 2000);
                }}
              >
                {copied ? "Copied!" : "Copy"}
              </button>
            </div>
          </div>
          <div className="mt-6 flex gap-3">
            <button
              type="button"
              className="rounded-full bg-zinc-900 px-5 py-3 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900"
              onClick={() => router.push("/")}
            >
              Back to markets
            </button>
            <button
              type="button"
              className="rounded-full border border-zinc-300 px-5 py-3 text-sm font-semibold text-zinc-700 dark:border-zinc-700 dark:text-zinc-200"
              onClick={() => {
                setSuccessSig("");
                setForm({ title: "", description: "", close_time: "", settle_time: "", oracle_feed_id: "", oracle_condition: "gte", oracle_target_price: "250.00" });
              }}
            >
              Create another
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen bg-stone-100 px-4 py-10 dark:bg-zinc-950 sm:px-6 lg:px-8">
      <div className="mx-auto max-w-3xl rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
        <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">
          Create a market
        </h1>
        <p className="mt-2 text-sm text-zinc-500 dark:text-zinc-400">
          v1a: anyone can create markets (requires login), matching stays disabled.
        </p>

        <div className="mt-6 grid gap-4 md:grid-cols-2">
          <Input
            label="Title"
            value={form.title}
            onChange={(value) => setForm((current) => ({ ...current, title: value }))}
          />
          <div>
            <label className="text-sm font-medium text-zinc-700 dark:text-zinc-200">
              Cover image
            </label>
            <div className="mt-2 flex items-center gap-3">
              <label className="cursor-pointer rounded-full bg-zinc-900 px-4 py-2 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900">
                Upload
                <input
                  className="hidden"
                  type="file"
                  accept="image/*"
                  onChange={async (event) => {
                    const file = event.target.files?.[0];
                    if (!file) return;
                    if (file.size > 250 * 1024) {
                      setError("Image too large (max 250KB for v1a).");
                      return;
                    }
                    const reader = new FileReader();
                    reader.onload = () => {
                      const result = typeof reader.result === "string" ? reader.result : "";
                      if (!result.startsWith("data:image/")) {
                        setError("Invalid image file.");
                        return;
                      }
                      setError("");
                      setImageDataUrl(result);
                    };
                    reader.readAsDataURL(file);
                  }}
                />
              </label>
              {imageDataUrl ? (
                <div className="flex items-center gap-3">
                  {/* eslint-disable-next-line @next/next/no-img-element */}
                  <img
                    alt="preview"
                    src={imageDataUrl}
                    className="h-12 w-12 rounded-2xl border border-zinc-200 object-cover dark:border-zinc-700"
                  />
                  <button
                    type="button"
                    className="text-sm font-medium text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
                    onClick={() => setImageDataUrl("")}
                  >
                    Remove
                  </button>
                </div>
              ) : (
                <div className="text-sm text-zinc-500 dark:text-zinc-400">
                  Optional
                </div>
              )}
            </div>
          </div>
          <Input
            label="Close time"
            type="datetime-local"
            value={form.close_time}
            onChange={(value) =>
              setForm((current) => ({ ...current, close_time: value }))
            }
          />
          <Input
            label="Earliest settle time"
            type="datetime-local"
            value={form.settle_time}
            onChange={(value) =>
              setForm((current) => ({ ...current, settle_time: value }))
            }
          />
          <div>
            <label className="text-sm font-medium text-zinc-700 dark:text-zinc-200">
              Resolution mode
            </label>
            <div className="mt-2 flex gap-2">
              {(["creator", "pyth"] as const).map((value) => (
                <button
                  key={value}
                  className={`rounded-full px-4 py-2 text-sm font-semibold ${
                    mode === value
                      ? "bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
                      : "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200"
                  }`}
                  onClick={() => setMode(value)}
                  type="button"
                >
                  {value}
                </button>
              ))}
            </div>
          </div>
        </div>

        {mode === "pyth" ? (
          <div className="mt-4 grid gap-4 md:grid-cols-2">
            <Input
              label="Pyth feed id (0x...)"
              value={form.oracle_feed_id}
              onChange={(value) =>
                setForm((current) => ({ ...current, oracle_feed_id: value.trim() }))
              }
            />
            <Select
              label="Oracle condition"
              value={form.oracle_condition}
              onChange={(value) =>
                setForm((current) => ({ ...current, oracle_condition: value }))
              }
              options={[
                { value: "gte", label: ">=" },
                { value: "gt", label: ">" },
                { value: "lt", label: "<" },
                { value: "lte", label: "<=" },
              ]}
            />
            <Input
              label="Oracle target price (decimal)"
              value={form.oracle_target_price}
              onChange={(value) =>
                setForm((current) => ({ ...current, oracle_target_price: value }))
              }
            />
            <div className="rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 text-sm dark:border-zinc-700 dark:bg-zinc-950">
              <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">
                Feed validation
              </div>
              <div className="mt-2 text-sm text-zinc-700 dark:text-zinc-200">
                {feedCheck.status === "idle" ? (
                  "Enter a feed id to validate."
                ) : feedCheck.status === "checking" ? (
                  "Checking Hermes..."
                ) : feedCheck.status === "invalid" ? (
                  <span className="font-medium text-rose-700 dark:text-rose-300">
                    Not valid: {feedCheck.reason}
                  </span>
                ) : (
                  <div className="space-y-1">
                    <div className="font-medium text-emerald-700 dark:text-emerald-300">
                      Valid feed id
                    </div>
                    <div className="text-xs text-zinc-500 dark:text-zinc-400">
                      Latest publish time:{" "}
                      {new Date(feedCheck.publishTime * 1000).toLocaleString()}
                    </div>
                    <div className="text-xs text-zinc-500 dark:text-zinc-400">
                      Latest price (decimal):{" "}
                      <span className="font-mono text-zinc-900 dark:text-zinc-100">
                        {feedCheck.priceDecimal}
                      </span>{" "}
                      (expo:{" "}
                      <span className="font-mono text-zinc-900 dark:text-zinc-100">
                        {feedCheck.expo}
                      </span>
                      )
                    </div>
                    <div className="text-xs text-zinc-500 dark:text-zinc-400">
                      Latest price (int @ expo):{" "}
                      <span className="font-mono text-zinc-900 dark:text-zinc-100">
                        {feedCheck.priceInt}
                      </span>
                    </div>
                    <div className="text-xs text-zinc-500 dark:text-zinc-400">
                      Target price (int @ expo):{" "}
                      <span className="font-mono text-zinc-900 dark:text-zinc-100">
                        {targetPriceIntAtExpo || "invalid"}
                      </span>
                    </div>
                    {targetPriceIntAtExpo ? (
                      <div className="text-xs text-zinc-500 dark:text-zinc-400">
                        Comparison preview:{" "}
                        <span className="font-medium text-zinc-900 dark:text-zinc-100">
                          {previewComparison(
                            BigInt(feedCheck.priceInt),
                            BigInt(targetPriceIntAtExpo),
                            form.oracle_condition,
                          )}
                        </span>
                      </div>
                    ) : null}
                  </div>
                )}
              </div>
            </div>
          </div>
        ) : null}

        <div className="mt-6">
          <label className="text-sm font-medium text-zinc-700 dark:text-zinc-200">
            Description
          </label>
          <textarea
            className="mt-2 h-40 w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 outline-none disabled:cursor-not-allowed disabled:opacity-80 dark:border-zinc-700 dark:bg-zinc-950"
            value={form.description}
            onChange={(event) =>
              setForm((current) => ({ ...current, description: event.target.value }))
            }
            disabled={mode === "pyth"}
            placeholder={
              mode === "pyth"
                ? "This will be auto-generated after Pyth inputs are valid."
                : "Required. Explain the resolution rule and time standard."
            }
          />
          {mode === "pyth" ? (
            <p className="mt-2 text-xs leading-5 text-zinc-500 dark:text-zinc-400">
              Auto-generated for Pyth markets to keep settlement rules consistent.
            </p>
          ) : (
            <p className="mt-2 text-xs leading-5 text-zinc-500 dark:text-zinc-400">
              Required. Explain the resolution rule and time standard (UTC vs local).
            </p>
          )}
        </div>

        {error ? (
          <div className="mt-4 rounded-2xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700 dark:border-rose-900/50 dark:bg-rose-900/20 dark:text-rose-200">
            {error}
          </div>
        ) : null}

        <div className="mt-6 flex gap-3">
          <button
            className="rounded-full bg-zinc-900 px-5 py-3 text-sm font-semibold text-white disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
            disabled={loading || !pythIsValid}
            onClick={async () => {
              try {
                if (!resolutionAuthority && !localWalletMode) {
                  setError("No wallet connected.");
                  return;
                }
                if (!form.title.trim()) {
                  setError("Title is required.");
                  return;
                }
                if (mode === "creator" && !form.description.trim()) {
                  setError("Description is required.");
                  return;
                }
                if (mode === "pyth" && !form.description.trim()) {
                  setError("Fill valid Pyth inputs to auto-generate the description.");
                  return;
                }
                if (!form.close_time) {
                  setError("Close time is required.");
                  return;
                }
                if (!form.settle_time) {
                  setError("Earliest settle time is required.");
                  return;
                }
                const closeTime = new Date(form.close_time).getTime();
                const settleTime = new Date(form.settle_time).getTime();
                if (Number.isNaN(closeTime) || Number.isNaN(settleTime)) {
                  setError("Invalid time value.");
                  return;
                }
                const now = Date.now();
                if (closeTime <= now + 30_000) {
                  setError("Close time must be at least 30 seconds in the future.");
                  return;
                }
                if (settleTime < closeTime) {
                  setError("Earliest settle time must be >= close time.");
                  return;
                }
                if (mode === "pyth") {
                  if (feedCheck.status !== "valid") {
                    setError("Pyth feed id is not valid.");
                    return;
                  }
                  if (!targetPriceIntAtExpo) {
                    setError("Oracle target price is invalid.");
                    return;
                  }
                }

                setLoading(true);
                setError("");
                const pinataJwt = process.env.NEXT_PUBLIC_PINATA_JWT || "";
                if (!pinataJwt) {
                  setError("Pinata JWT missing. Set NEXT_PUBLIC_PINATA_JWT.");
                  return;
                }

                let creatorAddress = resolutionAuthority;
                let localProvider:
                  | {
                      publicKey?: PublicKey;
                      connect?: () => Promise<void>;
                      signTransaction?: (tx: Transaction) => Promise<Transaction>;
                    }
                  | null = null;
                if (localWalletMode) {
                  localProvider = getLocalWalletProvider();
                  if (!localProvider) {
                    setError("Local wallet provider not found. Install Phantom.");
                    return;
                  }
                  if (!localProvider.publicKey && localProvider.connect) {
                    await localProvider.connect();
                  }
                  if (!localProvider.publicKey) {
                    setError("Local wallet is not connected.");
                    return;
                  }
                  creatorAddress = localProvider.publicKey.toBase58();
                }

                const imageUri = imageDataUrl
                  ? await pinImageDataUrl(imageDataUrl, pinataJwt)
                  : "";

                const closeISO = new Date(form.close_time).toISOString();
                const resolveISO = new Date(form.settle_time).toISOString();

                const metadata = buildMetadata({
                  title: form.title.trim(),
                  description: form.description.trim(),
                  imageUri,
                  category: "",
                  closeTime: closeISO,
                  resolveAfterTime: resolveISO,
                  resolution:
                    mode === "creator"
                      ? {
                          mode,
                          authority: creatorAddress,
                        }
                      : {
                          mode,
                          oracleFeedId: form.oracle_feed_id.trim().toLowerCase(),
                          oracleCondition: form.oracle_condition,
                          oracleTargetPrice: form.oracle_target_price.trim(),
                        },
                });

                const metadataUri = await pinJSONToIPFS(metadata, pinataJwt);
                const marketId = await computeMarketId({
                  creator: creatorAddress,
                  title: metadata.title,
                  closeTime: closeISO,
                  metadataUri,
                });

                const programId = new PublicKey(
                  process.env.NEXT_PUBLIC_PROGRAM_ID ||
                    "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
                );
                const collateralMint = process.env.NEXT_PUBLIC_VUSDC_MINT;
                if (!collateralMint) {
                  setError("NEXT_PUBLIC_VUSDC_MINT is required.");
                  return;
                }

                const priceInt = mode === "pyth" ? BigInt(targetPriceIntAtExpo) : BigInt(0);
                const priceExpo = mode === "pyth" && feedCheck.status === "valid" ? feedCheck.expo : 0;
                const oracleFeedId =
                  mode === "pyth"
                    ? hexToBytes32(form.oracle_feed_id.trim())
                    : new Uint8Array(32);

                const instruction = await buildInitializeMarketInstruction({
                  programId,
                  marketId,
                  metadataUri,
                  closeTime: BigInt(Math.floor(new Date(form.close_time).getTime() / 1000)),
                  resolveAfterTime: BigInt(Math.floor(new Date(form.settle_time).getTime() / 1000)),
                  resolutionMode: mode === "creator" ? 0 : 1,
                  oracleFeedId,
                  oracleCondition: mode === "pyth" ? conditionToIndex(form.oracle_condition) : 0,
                  oracleTargetPriceInt: priceInt,
                  oracleTargetExpo: priceExpo,
                  collateralMint: new PublicKey(collateralMint),
                  creator: new PublicKey(creatorAddress),
                });

                const connection = new Connection(
                  process.env.NEXT_PUBLIC_RPC_URL || "https://api.devnet.solana.com",
                );
                const latest = await connection.getLatestBlockhash();
                const tx = new Transaction({
                  feePayer: new PublicKey(creatorAddress),
                  recentBlockhash: latest.blockhash,
                }).add(instruction);

                if (localWalletMode) {
                  if (!localProvider || !localProvider.signTransaction) {
                    setError("Local wallet does not support signTransaction.");
                    return;
                  }
                  const signed = await localProvider.signTransaction(tx);
                  const sig = await connection.sendRawTransaction(signed.serialize());
                  await connection.confirmTransaction(sig, "confirmed");
                  setSuccessSig(sig);
                } else {
                  // 先模拟交易以获取详细错误信息
                  try {
                    console.log("Simulating transaction...");
                    const simulation = await connection.simulateTransaction(tx);
                    console.log("Simulation result:", simulation);

                    if (simulation.value.err) {
                      console.error("Simulation failed:", simulation.value.err);
                      console.error("Simulation logs:", simulation.value.logs);
                      setError(`Transaction simulation failed: ${JSON.stringify(simulation.value.err)}\nLogs: ${simulation.value.logs?.join('\n')}`);
                      setLoading(false);
                      return;
                    }
                    console.log("Simulation successful!");
                  } catch (simError) {
                    console.error("Simulation error:", simError);
                    setError(`Simulation error: ${simError instanceof Error ? simError.message : String(simError)}`);
                    setLoading(false);
                    return;
                  }

                  const raw = tx.serialize({ requireAllSignatures: false });
                  console.log("Transaction serialized, length:", raw.length);
                  console.log("Transaction base64:", Buffer.from(raw).toString('base64'));

                  if (!wallet) {
                    setError("No wallet connected.");
                    return;
                  }
                  console.log("Selected wallet:", wallet.address);

                  try {
                    console.log("Calling signAndSendTransaction...");
                    const result = await signAndSendTransaction({
                      transaction: new Uint8Array(raw),
                      wallet,
                      chain: "solana:devnet",
                    });
                    console.log("signAndSendTransaction result:", result);
                    console.log("Result JSON:", JSON.stringify(result, null, 2));

                    if (result?.signature) {
                      const normalizedSignature = normalizeTransactionSignature(result.signature);
                      console.log("Success! Signature:", normalizedSignature);
                      setSuccessSig(normalizedSignature);
                    } else {
                      console.error("No signature in result:", result);
                      setError("Transaction sent but no signature returned");
                      setLoading(false);
                      return;
                    }
                  } catch (txError) {
                    console.error("signAndSendTransaction error:", txError);
                    throw txError;
                  }
                }

              } catch (error: unknown) {
                console.error("Full error object:", error);
                const message =
                  error instanceof Error ? error.message : "Failed to create market";
                setError(message);
              } finally {
                setLoading(false);
              }
            }}
            type="button"
          >
            {loading ? "Saving..." : "Create market"}
          </button>
        </div>
      </div>
    </div>
  );
}

function normalizeTransactionSignature(signature: string | Uint8Array): string {
  if (typeof signature === "string") {
    return signature;
  }
  return Buffer.from(signature).toString("base64");
}

const Input = ({
  label,
  value,
  onChange,
  type = "text",
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  type?: string;
}) => (
  <div>
    <label className="text-sm font-medium text-zinc-700 dark:text-zinc-200">
      {label}
    </label>
    <input
      className="mt-2 w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 outline-none dark:border-zinc-700 dark:bg-zinc-950"
      type={type}
      value={value}
      onChange={(event) => onChange(event.target.value)}
    />
  </div>
);

const Select = ({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: "gte" | "gt" | "lt" | "lte";
  onChange: (value: "gte" | "gt" | "lt" | "lte") => void;
  options: Array<{ value: "gte" | "gt" | "lt" | "lte"; label: string }>;
}) => (
  <div>
    <label className="text-sm font-medium text-zinc-700 dark:text-zinc-200">
      {label}
    </label>
    <select
      className="mt-2 w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 outline-none dark:border-zinc-700 dark:bg-zinc-950"
      value={value}
      onChange={(event) => onChange(event.target.value as "gte" | "gt" | "lt" | "lte")}
    >
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  </div>
);

function decimalToPythPriceInt(value: string, expo: number): string {
  // Convert a human decimal string into Pyth's (price_int, expo) integer representation.
  // Pyth defines: price_decimal = price_int * 10^expo.
  // So: price_int = price_decimal * 10^(-expo).
  const raw = value.trim();
  if (!raw) return "";
  if (!/^\d+(\.\d+)?$/.test(raw)) return "";

  const [intPartRaw, fracRaw = ""] = raw.split(".");
  const intPart = intPartRaw.replace(/^0+(?=\d)/, "");
  const frac = fracRaw.replace(/0+$/, "");
  const numerator = BigInt((intPart || "0") + frac);
  const fracDigits = frac.length;

  // price_decimal = numerator / 10^fracDigits
  // price_int = numerator * 10^(-expo) / 10^fracDigits
  if (expo <= 0) {
    const multiplier = pow10BigInt(-expo);
    const denom = pow10BigInt(fracDigits);
    const scaled = numerator * multiplier;
    if (scaled % denom !== BigInt(0)) {
      // Too many decimals for this expo; refuse to silently round.
      return "";
    }
    return (scaled / denom).toString();
  }

  // expo > 0 => divide by 10^expo. We require an exact integer result.
  const denom = pow10BigInt(fracDigits + expo);
  if (numerator % denom !== BigInt(0)) {
    return "";
  }
  return (numerator / denom).toString();
}

type LocalWalletProvider = {
  publicKey?: PublicKey;
  connect?: () => Promise<void>;
  signTransaction?: (tx: Transaction) => Promise<Transaction>;
  isPhantom?: boolean;
};

function getLocalWalletProvider(): LocalWalletProvider | null {
  if (typeof window === "undefined") return null;
  const provider = (window as { solana?: LocalWalletProvider }).solana;
  if (!provider) return null;
  return provider;
}

function getLocalWalletAddress(): string {
  const provider = getLocalWalletProvider();
  if (!provider || !provider.publicKey) return "";
  return provider.publicKey.toBase58();
}

type MetadataPayload = {
  title: string;
  description: string;
  image?: string;
  category: string;
  close_time: string;
  resolve_after_time: string;
  resolution: {
    mode: "creator" | "pyth";
    authority?: string;
    oracle_feed_id?: string;
    oracle_condition?: "gte" | "gt" | "lt" | "lte";
    oracle_target_price?: string;
  };
  version: string;
};

function buildMetadata(input: {
  title: string;
  description: string;
  imageUri: string;
  category: string;
  closeTime: string;
  resolveAfterTime: string;
  resolution:
    | { mode: "creator"; authority: string }
    | {
        mode: "pyth";
        oracleFeedId: string;
        oracleCondition: "gte" | "gt" | "lt" | "lte";
        oracleTargetPrice: string;
      };
}): MetadataPayload {
  return {
    title: input.title,
    description: input.description,
    image: input.imageUri || undefined,
    category: input.category,
    close_time: input.closeTime,
    resolve_after_time: input.resolveAfterTime,
    resolution:
      input.resolution.mode === "creator"
        ? {
            mode: "creator",
            authority: input.resolution.authority,
          }
        : {
            mode: "pyth",
            oracle_feed_id: input.resolution.oracleFeedId,
            oracle_condition: input.resolution.oracleCondition,
            oracle_target_price: input.resolution.oracleTargetPrice,
          },
    version: "1.0",
  };
}

async function pinImageDataUrl(dataUrl: string, jwt: string): Promise<string> {
  const response = await fetch(dataUrl);
  const blob = await response.blob();
  const file = new File([blob], "market-cover", { type: blob.type || "image/png" });
  const cid = await pinFileToIPFS(file, jwt);
  return `ipfs://${cid}`;
}

async function pinFileToIPFS(file: File, jwt: string): Promise<string> {
  const formData = new FormData();
  formData.append("file", file);

  const response = await fetch("https://api.pinata.cloud/pinning/pinFileToIPFS", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${jwt}`,
    },
    body: formData,
  });
  if (!response.ok) {
    throw new Error(`Pinata file upload failed (${response.status})`);
  }
  const json = (await response.json()) as { IpfsHash?: string };
  if (!json.IpfsHash) {
    throw new Error("Pinata file upload did not return IpfsHash");
  }
  return json.IpfsHash;
}

async function pinJSONToIPFS(payload: MetadataPayload, jwt: string): Promise<string> {
  const response = await fetch("https://api.pinata.cloud/pinning/pinJSONToIPFS", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${jwt}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    throw new Error(`Pinata JSON upload failed (${response.status})`);
  }
  const json = (await response.json()) as { IpfsHash?: string };
  if (!json.IpfsHash) {
    throw new Error("Pinata JSON upload did not return IpfsHash");
  }
  return `ipfs://${json.IpfsHash}`;
}

async function computeMarketId(input: {
  creator: string;
  title: string;
  closeTime: string;
  metadataUri: string;
}): Promise<bigint> {
  const encoder = new TextEncoder();
  const seed = `${input.creator}|${input.title}|${input.closeTime}|${input.metadataUri}`;
  const hash = await sha256Bytes(encoder.encode(seed));
  let id = BigInt(0);
  for (let i = 0; i < 8; i += 1) {
    id |= BigInt(hash[i]) << BigInt(8 * i);
  }
  return id;
}

async function sha256Bytes(data: Uint8Array): Promise<Uint8Array> {
  const normalized = new Uint8Array(data.byteLength);
  normalized.set(data);
  const digest = await crypto.subtle.digest("SHA-256", normalized);
  return new Uint8Array(digest);
}

function hexToBytes32(value: string): Uint8Array {
  const raw = value.startsWith("0x") ? value.slice(2) : value;
  if (!/^[0-9a-fA-F]{64}$/.test(raw)) {
    throw new Error("Pyth feed id must be 0x + 64 hex chars");
  }
  return Uint8Array.from(Buffer.from(raw, "hex"));
}

function conditionToIndex(condition: "gte" | "gt" | "lt" | "lte"): number {
  switch (condition) {
    case "gt":
      return 0;
    case "gte":
      return 1;
    case "lt":
      return 2;
    case "lte":
      return 3;
    default:
      return 1;
  }
}

type InitializeMarketArgs = {
  programId: PublicKey;
  marketId: bigint;
  metadataUri: string;
  closeTime: bigint;
  resolveAfterTime: bigint;
  resolutionMode: number;
  oracleFeedId: Uint8Array;
  oracleCondition: number;
  oracleTargetPriceInt: bigint;
  oracleTargetExpo: number;
  collateralMint: PublicKey;
  creator: PublicKey;
};

async function buildInitializeMarketInstruction(args: InitializeMarketArgs): Promise<TransactionInstruction> {
  const marketIdBytes = u64ToBytesLE(args.marketId);
  const [marketPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("market"), Buffer.from(marketIdBytes)],
    args.programId,
  );
  const [vaultPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("collateral_vault"), Buffer.from(marketIdBytes)],
    args.programId,
  );
  const [yesMintPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("yes_mint"), Buffer.from(marketIdBytes)],
    args.programId,
  );
  const [noMintPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("no_mint"), Buffer.from(marketIdBytes)],
    args.programId,
  );

  const data = await encodeInitializeMarketData({
    marketId: args.marketId,
    metadataUri: args.metadataUri,
    closeTime: args.closeTime,
    resolveAfterTime: args.resolveAfterTime,
    resolutionMode: args.resolutionMode,
    oracleFeedId: args.oracleFeedId,
    oracleCondition: args.oracleCondition,
    oracleTargetPriceInt: args.oracleTargetPriceInt,
    oracleTargetExpo: args.oracleTargetExpo,
  });

  return new TransactionInstruction({
    programId: args.programId,
    keys: [
      { pubkey: marketPda, isSigner: false, isWritable: true },
      { pubkey: vaultPda, isSigner: false, isWritable: true },
      { pubkey: yesMintPda, isSigner: false, isWritable: true },
      { pubkey: noMintPda, isSigner: false, isWritable: true },
      { pubkey: args.collateralMint, isSigner: false, isWritable: false },
      { pubkey: args.creator, isSigner: true, isWritable: true },
      { pubkey: TOKEN_2022_PROGRAM_ID, isSigner: false, isWritable: false },
      { pubkey: ASSOCIATED_TOKEN_PROGRAM_ID, isSigner: false, isWritable: false },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
    ],
    data: Buffer.from(data),
  });
}

async function encodeInitializeMarketData(input: {
  marketId: bigint;
  metadataUri: string;
  closeTime: bigint;
  resolveAfterTime: bigint;
  resolutionMode: number;
  oracleFeedId: Uint8Array;
  oracleCondition: number;
  oracleTargetPriceInt: bigint;
  oracleTargetExpo: number;
}): Promise<Uint8Array> {
  const discriminator = await anchorDiscriminator("initialize_market");
  const parts = [
    discriminator,
    u64ToBytesLE(input.marketId),
    encodeString(input.metadataUri),
    i64ToBytesLE(input.closeTime),
    i64ToBytesLE(input.resolveAfterTime),
    Uint8Array.of(input.resolutionMode),
    input.oracleFeedId,
    Uint8Array.of(input.oracleCondition),
    u64ToBytesLE(input.oracleTargetPriceInt),
    i32ToBytesLE(input.oracleTargetExpo),
  ];
  return concatBytes(parts);
}

async function anchorDiscriminator(name: string): Promise<Uint8Array> {
  const encoder = new TextEncoder();
  const preimage = encoder.encode(`global:${name}`);
  const hash = await sha256Bytes(preimage);
  return hash.slice(0, 8);
}

function encodeString(value: string): Uint8Array {
  const encoder = new TextEncoder();
  const bytes = encoder.encode(value);
  return concatBytes([u32ToBytesLE(bytes.length), bytes]);
}

function u32ToBytesLE(value: number): Uint8Array {
  const out = new Uint8Array(4);
  out[0] = value & 0xff;
  out[1] = (value >> 8) & 0xff;
  out[2] = (value >> 16) & 0xff;
  out[3] = (value >> 24) & 0xff;
  return out;
}

function u64ToBytesLE(value: bigint): Uint8Array {
  const out = new Uint8Array(8);
  let v = BigInt.asUintN(64, value);
  for (let i = 0; i < 8; i += 1) {
    out[i] = Number(v & BigInt(0xff));
    v >>= BigInt(8);
  }
  return out;
}

function i64ToBytesLE(value: bigint): Uint8Array {
  const out = new Uint8Array(8);
  let v = BigInt.asIntN(64, value);
  for (let i = 0; i < 8; i += 1) {
    out[i] = Number(v & BigInt(0xff));
    v >>= BigInt(8);
  }
  return out;
}

function i32ToBytesLE(value: number): Uint8Array {
  const out = new Uint8Array(4);
  const v = value | 0;
  out[0] = v & 0xff;
  out[1] = (v >> 8) & 0xff;
  out[2] = (v >> 16) & 0xff;
  out[3] = (v >> 24) & 0xff;
  return out;
}

function concatBytes(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function formatPythPrice(priceInt: string, expo: number): string {
  // priceInt is an integer with decimal exponent `expo` (typically negative).
  const negative = priceInt.startsWith("-");
  const digits = (negative ? priceInt.slice(1) : priceInt).replace(/^0+(?=\d)/, "");
  if (expo === 0) return (negative ? "-" : "") + digits;
  if (expo > 0) return (negative ? "-" : "") + digits + "0".repeat(expo);

  const places = -expo;
  const padded = digits.padStart(places + 1, "0");
  const splitAt = padded.length - places;
  const whole = padded.slice(0, splitAt);
  const frac = padded.slice(splitAt).replace(/0+$/, "");
  return (negative ? "-" : "") + (frac ? `${whole}.${frac}` : whole);
}

function previewComparison(
  currentInt: bigint,
  targetInt: bigint,
  condition: "gte" | "gt" | "lt" | "lte",
): string {
  const yesWins = (() => {
    switch (condition) {
      case "gt":
        return currentInt > targetInt;
      case "gte":
        return currentInt >= targetInt;
      case "lt":
        return currentInt < targetInt;
      case "lte":
        return currentInt <= targetInt;
    }
  })();

  const op = condition === "gt" ? ">" : condition === "gte" ? ">=" : condition === "lt" ? "<" : "<=";
  return `current ${op} target => ${yesWins ? "YES wins" : "NO wins"}`;
}

function pow10BigInt(exp: number): bigint {
  // Avoid BigInt literals (n-suffix) to keep TS builds happy under lower targets.
  let result = BigInt(1);
  const ten = BigInt(10);
  for (let i = 0; i < exp; i += 1) {
    result *= ten;
  }
  return result;
}

function normalizeFeedId(value: string): string {
  const raw = value.trim().toLowerCase();
  return raw.startsWith("0x") ? raw.slice(2) : raw;
}

function buildPythDescription(input: {
  title: string;
  closeTimeISO: string;
  earliestSettleTimeISO: string;
  feedId: string;
  oracleConditionSymbol: string;
  targetPriceDecimal: string;
}): string {
  // Keep this deterministic and explicit so traders can reason about settlement.
  // We store ISO timestamps (UTC) to avoid local-time ambiguity in the generated copy.
  const id = input.feedId.toLowerCase().startsWith("0x")
    ? input.feedId.toLowerCase()
    : `0x${input.feedId.toLowerCase()}`;

  return [
    `Settlement for "${input.title}":`,
    ``,
    `- Trading closes at: ${input.closeTimeISO}`,
    `- Earliest settle time: ${input.earliestSettleTimeISO}`,
    `- Oracle: Pyth price feed id ${id}`,
    ``,
    `After the earliest settle time, a scheduled settlement task will attempt to resolve this market.`,
    `Observed price is strictly the first available Pyth price update where publish_time >= earliest settle time.`,
    `The program evaluates whether the observed price ${input.oracleConditionSymbol} ${input.targetPriceDecimal}.`,
    `If true, YES wins; otherwise, NO wins.`,
  ].join("\n");
}
