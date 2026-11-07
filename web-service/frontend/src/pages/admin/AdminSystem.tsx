import { useServiceStatus } from "../../api/hooks";
import { ErrorNote, Loading } from "../../components/ui";

export function AdminSystem() {
  const status = useServiceStatus();
  if (status.isPending) return <Loading />;
  if (status.isError) return <ErrorNote error={status.error} />;
  return (
    <>
      <p className="mb-4 text-sm text-muted">Each service's health check, as seen from the web gateway, every few seconds. Stop one and watch what the shop does.</p>
      <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {status.data.map((s) => (
          <li key={s.name} className="panel p-4">
            <p className="font-display text-lg font-bold">{s.name}</p>
            <p className={`mt-1 text-sm font-semibold ${s.up ? "text-ok" : "text-bad"}`}>{s.up ? "● Up" : "✕ Down"}</p>
            <p className="text-xs text-muted">{s.up ? `${s.latency_ms} ms` : "no answer"}</p>
          </li>
        ))}
      </ul>
    </>
  );
}
