import { NextRequest, NextResponse } from "next/server";

export async function GET(req: NextRequest) {
  const raw = req.nextUrl.searchParams.get("url") || "";
  const url = normalizeMetadataURL(raw);
  if (!url) {
    return NextResponse.json({ message: "invalid metadata url" }, { status: 400 });
  }

  try {
    const response = await fetch(url, {
      method: "GET",
      cache: "no-store",
      headers: { Accept: "application/json,text/plain,*/*" },
    });
    if (!response.ok) {
      return NextResponse.json(
        { message: `metadata fetch failed (${response.status})`, resolved_url: url },
        { status: response.status },
      );
    }

    const json = (await response.json()) as unknown;
    return NextResponse.json({ data: json, resolved_url: url });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "metadata fetch failed";
    return NextResponse.json({ message, resolved_url: url }, { status: 502 });
  }
}

function normalizeMetadataURL(raw: string): string {
  const value = raw.trim();
  if (!value) return "";

  if (value.startsWith("ipfs://")) {
    const path = value.slice("ipfs://".length).replace(/^ipfs\//, "");
    return `https://ipfs.io/ipfs/${path}`;
  }

  try {
    const parsed = new URL(value);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
    return parsed.toString();
  } catch {
    return "";
  }
}

