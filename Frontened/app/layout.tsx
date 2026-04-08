import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import Providers from "@/components/provider";
import { Toaster } from "sonner";
import { AppHeader } from "@/components/app-header";

const geistSans = Geist({ variable: "--font-geist-sans", subsets: ["latin"] });
const geistMono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  title: "SolPredict v1a",
  description: "SolPredict skeleton frontend for Solana prediction markets",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className={`${geistSans.variable} ${geistMono.variable} bg-stone-100 text-zinc-950 antialiased dark:bg-zinc-950 dark:text-zinc-50`}>
        <Providers>
          <Toaster richColors position="top-center" />
          <AppHeader />
          {children}
        </Providers>
      </body>
    </html>
  );
}
