import Link from "next/link";

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen bg-stone-100 dark:bg-zinc-950">
      <header className="border-b border-black/5 bg-white/80 backdrop-blur dark:border-white/10 dark:bg-zinc-950/80">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-4 py-4 sm:px-6 lg:px-8">
          <div>
            <div className="text-xs uppercase tracking-[0.3em] text-zinc-500 dark:text-zinc-400">BlinkPredict</div>
            <div className="text-lg font-semibold">Admin Console</div>
          </div>
          <nav className="flex gap-3 text-sm font-medium">
            <Link href="/admin/markets" className="text-zinc-600 hover:text-zinc-950 dark:text-zinc-300 dark:hover:text-zinc-50">Markets</Link>
            <Link href="/markets/create" className="text-zinc-600 hover:text-zinc-950 dark:text-zinc-300 dark:hover:text-zinc-50">Create</Link>
          </nav>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-8 sm:px-6 lg:px-8">{children}</main>
    </div>
  );
}
