export function PlaceholderPage({ title }: { title: string }) {
  return (
    <div className="text-muted-foreground">
      <h1 className="mb-2 text-xl font-semibold text-foreground">{title}</h1>
      <p>功能开发中。</p>
    </div>
  );
}
