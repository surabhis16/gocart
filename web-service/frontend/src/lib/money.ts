const formatters = new Map<string, Intl.NumberFormat>();

/** Prices are integers in the smallest unit (paise). Never do arithmetic on floats. */
export function formatMoney(minor: number, currency = "INR"): string {
  let f = formatters.get(currency);
  if (!f) {
    f = new Intl.NumberFormat("en-IN", { style: "currency", currency, minimumFractionDigits: 2 });
    formatters.set(currency, f);
  }
  return f.format(minor / 100);
}
