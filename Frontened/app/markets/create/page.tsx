"use client";

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import api from "@/app/utils/axiosInstance";
import { useIdentityToken } from "@privy-io/react-auth";
import { useWallets } from "@privy-io/react-auth/solana";

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
  const { identityToken } = useIdentityToken();
  const { wallets } = useWallets();
  const [loading, setLoading] = useState(false);
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

  const resolutionAuthority = wallets?.[0]?.address || "";

  const targetPriceIntAtExpo = useMemo(() => {
    if (mode !== "pyth") return "";
    if (feedCheck.status !== "valid") return "";
    return decimalToPythPriceInt(form.oracle_target_price, feedCheck.expo);
  }, [feedCheck, form.oracle_target_price, mode]);

  const payload = useMemo(
    () => ({
      title: form.title,
      description: form.description,
      // v1a: metadata/image are still evolving; keep contract stable and avoid forcing users to paste URLs.
      metadata_url: "",
      image_url: imageDataUrl,
      close_time: form.close_time ? new Date(form.close_time).toISOString() : "",
      // v1a schema only has oracle_observation_time under resolution; we reuse it as the universal "earliest settle time".
      resolution:
        mode === "creator"
          ? {
              mode,
              authority: resolutionAuthority,
              oracle_observation_time: form.settle_time
                ? new Date(form.settle_time).toISOString()
                : "",
            }
          : {
              mode,
              oracle_feed: form.oracle_feed_id,
              oracle_condition: form.oracle_condition,
              // NOTE: In pull-mode designs, it is safer to store target as Pyth-style (price_int, expo)
              // to avoid precision loss. We currently send the int-only form; future backend/contract
              // can also store the expo to lock semantics.
              oracle_target_price: Number(targetPriceIntAtExpo || "0"),
              oracle_observation_time: form.settle_time
                ? new Date(form.settle_time).toISOString()
                : "",
            },
    }),
    [form, imageDataUrl, mode, resolutionAuthority, targetPriceIntAtExpo],
  );

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
                // Basic UX validation on the client; the API should also validate.
                if (!identityToken) {
                  setError("Please connect and login first.");
                  return;
                }
                if (!resolutionAuthority) {
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
                  const maybeUnsafe = BigInt(targetPriceIntAtExpo) > BigInt(Number.MAX_SAFE_INTEGER);
                  if (maybeUnsafe) {
                    setError("Oracle target price is too large to safely send as JSON number in v1a.");
                    return;
                  }
                }

                setLoading(true);
                setError("");
                await api.post("/markets", payload, {
                  headers: { "privy-id-token": identityToken },
                });
                router.push("/");
              } catch (error: unknown) {
                const message =
                  error instanceof Error ? error.message : "Failed to create market";
                const fallback =
                  typeof error === "object" &&
                  error !== null &&
                  "response" in error
                    ? (error as { response?: { data?: { message?: string } } }).response?.data
                        ?.message
                    : undefined;
                setError(fallback || message);
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
