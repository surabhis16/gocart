import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  useCart,
  useClearWishlist,
  useProducts,
  useRemoveWishlistItem,
  useSetItem,
  useWishlist,
} from "../api/hooks";
import type { Product, WishlistItem } from "../api/types";
import {
  Button,
  EmptyState,
  ErrorNote,
  Icon,
  Loading,
  Notice,
  PageTitle,
} from "../components/ui";
import { ProductArt } from "../components/ProductArt";
import { activeLines, MAX_QUANTITY } from "../lib/cart";
import { useCustomerId } from "../lib/identity";
import { formatMoney } from "../lib/money";
import type { Tone } from "../lib/orderFlow";

function Stock({ stock }: { stock: number }) {
  if (stock <= 0) return <span className="text-sm font-medium text-bad">Sold out</span>;
  if (stock <= 5) return <span className="text-sm font-medium text-warn">Only {stock} left</span>;
  return <span className="text-sm text-muted">{stock} in stock</span>;
}

interface ItemRowProps {
  item: WishlistItem;
  product?: Product;
  inCartQty: number;
  cartReady: boolean;
  cartPending: boolean;
  busy: boolean;
  moving: boolean;
  onAddToCart: () => void;
  onMoveToCart: () => void;
  onRemove: () => void;
}

function WishlistItemRow({
  item,
  product,
  inCartQty,
  cartReady,
  cartPending,
  busy,
  moving,
  onAddToCart,
  onMoveToCart,
  onRemove,
}: ItemRowProps) {
  if (!product) {
    return (
      <li className="flex flex-wrap items-center justify-between gap-4 py-4">
        <div className="flex min-w-0 flex-1 items-center gap-4">
          <div className="flex h-16 w-16 flex-shrink-0 items-center justify-center rounded-md border border-dashed border-ink/30 bg-sunken text-muted sm:h-20 sm:w-20">
            <Icon name="box" className="h-6 w-6 opacity-40" />
          </div>
          <div className="min-w-0">
            <p className="font-semibold text-muted">Unavailable product</p>
            <p className="font-mono text-xs text-muted">{item.product_id}</p>
            <p className="mt-1 text-sm font-medium text-bad">
              This product is no longer available in the catalogue.
            </p>
            <p className="mt-0.5 text-xs text-muted">Saved quantity: {item.quantity}</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            onClick={onRemove}
            disabled={busy}
            aria-label={`Remove unavailable product ${item.product_id} from wishlist`}
          >
            <Icon name="trash" className="h-4 w-4" />
            <span className="text-xs">Remove</span>
          </Button>
        </div>
      </li>
    );
  }

  const soldOut = product.stock <= 0;
  const maxAllowed = Math.min(product.stock, MAX_QUANTITY);
  const alreadyAtMax = inCartQty >= maxAllowed;
  const canAddToCart = cartReady && !soldOut && !alreadyAtMax;

  const actionText = (action: "add" | "move") => {
    if (cartPending) return "Loading cart…";
    if (!cartReady) return "Cart unavailable";
    if (soldOut) return "Sold out";
    if (alreadyAtMax) return "Max in cart";
    return action === "add" ? "Add to cart" : "Move to cart";
  };

  return (
    <li className="flex flex-wrap items-center justify-between gap-4 py-4">
      <div className="flex min-w-0 flex-1 items-center gap-4">
        <ProductArt
          id={product.id}
          className="h-16 w-16 flex-shrink-0 rounded-md border border-ink/20 sm:h-20 sm:w-20"
        />
        <div className="min-w-0">
          <p className="font-semibold text-base leading-snug">{product.name}</p>
          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-sm">
            <span className="font-display font-bold text-brand">
              {formatMoney(product.price_minor, product.currency)}
            </span>
            <span className="text-ink/30">·</span>
            <Stock stock={product.stock} />
            <span className="text-ink/30">·</span>
            <span className="text-xs text-muted">
              Saved quantity: <span className="font-semibold text-ink">{item.quantity}</span>
            </span>
            {cartReady && inCartQty > 0 && (
              <>
                <span className="text-ink/30">·</span>
                <span className="text-xs text-muted">({inCartQty} in cart)</span>
              </>
            )}
          </div>
          {product.description && (
            <p className="mt-1 line-clamp-1 text-xs text-muted">{product.description}</p>
          )}
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="secondary"
          onClick={onAddToCart}
          disabled={busy || !canAddToCart}
          aria-label={`Add ${product.name} to cart`}
        >
          {actionText("add")}
        </Button>
        <Button
          variant="primary"
          onClick={onMoveToCart}
          disabled={busy || !canAddToCart}
          busy={moving}
          aria-label={`Move ${product.name} to cart`}
        >
          {actionText("move")}
        </Button>
        <Button
          variant="ghost"
          className="!px-2.5"
          onClick={onRemove}
          disabled={busy}
          aria-label={`Remove ${product.name} from wishlist`}
        >
          <Icon name="trash" className="h-4 w-4" />
        </Button>
      </div>
    </li>
  );
}

export function WishlistPage() {
  const customer = useCustomerId();
  const wishlist = useWishlist(customer);
  const products = useProducts();
  const cart = useCart(customer);
  const setItem = useSetItem(customer);
  const removeWishlistItem = useRemoveWishlistItem(customer);
  const clearWishlist = useClearWishlist(customer);

  const [movingId, setMovingId] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ tone: Tone; title: string; message?: string } | null>(null);

  const items = wishlist.data?.items ?? [];
  const byId = useMemo(
    () => new Map((products.data ?? []).map((p) => [p.id, p])),
    [products.data],
  );

  const cartLines = activeLines(cart.data);
  const inCart = useMemo(
    () => new Map(cartLines.map((l) => [l.product_id, l.quantity])),
    [cartLines],
  );

  // 1. Loading and error states for Wishlist
  if (wishlist.isPending) return <Loading label="Loading your wishlist" />;
  if (wishlist.isError) return <ErrorNote error={wishlist.error} onRetry={() => wishlist.refetch()} />;

  // 2. Empty state when customer has no saved items
  if (items.length === 0) {
    return (
      <>
        <PageTitle title="Wishlist" />
        {notice && (
          <div className="mb-6">
            <Notice tone={notice.tone} title={notice.title}>
              {notice.message}
            </Notice>
          </div>
        )}
        <EmptyState title="Your wishlist is empty">
          <div className="flex items-center justify-center gap-3">
            <Link to="/" className="font-semibold text-brand underline">
              Browse the shop
            </Link>

            <span className="text-muted">·</span>

            <Link to="/cart" className="font-semibold text-brand underline">
              View your cart
            </Link>
          </div>
        </EmptyState>
      </>
    );
  }

  // 3. Catalogue loading and error states (only checked if there are items to display)
  if (products.isPending) return <Loading label="Loading the catalogue" />;
  if (products.isError) return <ErrorNote error={products.error} onRetry={() => products.refetch()} />;

  const cartReady = cart.isSuccess;
  const cartPending = cart.isPending;

  const mutating =
    setItem.isPending ||
    removeWishlistItem.isPending ||
    clearWishlist.isPending ||
    movingId !== null;

  const isBusy = (productId: string) =>
    movingId === productId ||
    (setItem.isPending && setItem.variables?.productId === productId) ||
    (removeWishlistItem.isPending && removeWishlistItem.variables === productId) ||
    clearWishlist.isPending;

  const handleAddToCart = (item: WishlistItem, product: Product) => {
    if (!cartReady) {
      setNotice({
        tone: "warn",
        title: "Cart is not ready",
        message: "Please wait until your cart has finished loading before adding items.",
      });
      return;
    }
    setNotice(null);
    const existingQty = inCart.get(item.product_id) ?? 0;
    const maxAllowed = Math.min(product.stock, MAX_QUANTITY);
    const targetQty = Math.min(existingQty + item.quantity, maxAllowed);

    if (targetQty <= existingQty) {
      setNotice({
        tone: "warn",
        title: "Cannot add more to cart",
        message: `You already have the maximum available quantity (${existingQty}) of ${product.name} in your cart.`,
      });
      return;
    }

    setItem.mutate(
      { productId: item.product_id, quantity: targetQty },
      {
        onSuccess: () => {
          setNotice({
            tone: "ok",
            title: "Added to cart",
            message: `Added ${product.name} to your cart (total in cart: ${targetQty}).`,
          });
        },
        onError: (err) => {
          setNotice({
            tone: "bad",
            title: "Could not add to cart",
            message: err.message || `Failed to add ${product.name} to your cart.`,
          });
        },
      },
    );
  };

  const handleMoveToCart = async (item: WishlistItem, product: Product) => {
    if (!cartReady) {
      setNotice({
        tone: "warn",
        title: "Cart is not ready",
        message: "Please wait until your cart has finished loading before moving items.",
      });
      return;
    }
    setNotice(null);
    const existingQty = inCart.get(item.product_id) ?? 0;
    const maxAllowed = Math.min(product.stock, MAX_QUANTITY);
    const targetQty = Math.min(existingQty + item.quantity, maxAllowed);

    if (targetQty <= existingQty) {
      setNotice({
        tone: "warn",
        title: "Cannot add more to cart",
        message: `You already have the maximum available quantity (${existingQty}) of ${product.name} in your cart.`,
      });
      return;
    }

    setMovingId(item.product_id);
    try {
      // Step 1: Write to Cart FIRST
      await setItem.mutateAsync({ productId: item.product_id, quantity: targetQty });

      // Step 2: Only remove from Wishlist after the Cart operation succeeds
      try {
        await removeWishlistItem.mutateAsync(item.product_id);
        setNotice({
          tone: "ok",
          title: "Moved to cart",
          message: `Moved ${product.name} to your cart.`,
        });
      } catch {
        // Step 3: Cart succeeded, but Wishlist removal failed -> keep both copies and show clear partial-success message
        setNotice({
          tone: "warn",
          title: "Partially completed",
          message: `Added ${product.name} to your cart, but could not remove it from your wishlist. Both copies have been kept; you can remove it manually.`,
        });
      }
    } catch {
      // Step 4: If Cart fails, do NOT remove the Wishlist item
      setNotice({
        tone: "bad",
        title: "Could not add to cart",
        message: `Failed to add ${product.name} to your cart. The item remains safely in your wishlist.`,
      });
    } finally {
      setMovingId(null);
    }
  };

  const handleRemove = (productId: string, productName?: string) => {
    setNotice(null);
    removeWishlistItem.mutate(productId, {
      onSuccess: () => {
        setNotice({
          tone: "ok",
          title: "Item removed",
          message: productName
            ? `Removed ${productName} from your wishlist.`
            : "Removed item from your wishlist.",
        });
      },
    });
  };

  return (
    <>
      <PageTitle title="Wishlist">
        <Button
          variant="ghost"
          onClick={() => {
            setNotice(null);
            clearWishlist.mutate(undefined, {
              onSuccess: () => {
                setNotice({
                  tone: "ok",
                  title: "Wishlist cleared",
                  message: "All items have been removed from your wishlist.",
                });
              },
            });
          }}
          disabled={mutating || items.length === 0}
        >
          Clear wishlist
        </Button>
      </PageTitle>

      {notice && (
        <div className="mb-6">
          <Notice tone={notice.tone} title={notice.title}>
            {notice.message}
          </Notice>
        </div>
      )}

      {(setItem.error || removeWishlistItem.error || clearWishlist.error) && (
        <div className="mb-6">
          <ErrorNote
            error={setItem.error ?? removeWishlistItem.error ?? clearWishlist.error}
          />
        </div>
      )}

      {cart.isError && (
        <div className="mb-6">
          <ErrorNote
            error={cart.error}
            onRetry={() => cart.refetch()}
          />
        </div>
      )}

      <ul className="panel divide-y divide-dotted divide-ink/30 px-5" aria-label="Saved items">
        {items.map((it) => {
          const product = byId.get(it.product_id);
          const inCartQty = inCart.get(it.product_id) ?? 0;
          return (
            <WishlistItemRow
              key={it.product_id}
              item={it}
              product={product}
              inCartQty={inCartQty}
              cartReady={cartReady}
              cartPending={cartPending}
              busy={isBusy(it.product_id)}
              moving={movingId === it.product_id}
              onAddToCart={() => product && handleAddToCart(it, product)}
              onMoveToCart={() => product && handleMoveToCart(it, product)}
              onRemove={() => handleRemove(it.product_id, product?.name)}
            />
          );
        })}
      </ul>

      <p className="mt-3 text-sm text-muted">
        Items saved here are stored for later. When you move an item to your cart, your saved quantity is preserved.
      </p>
    </>
  );
}
