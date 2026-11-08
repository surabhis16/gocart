import { Route, Routes } from "react-router-dom";
import { Layout } from "./components/Layout";
import { Landing } from "./pages/Landing";
import { Shop } from "./pages/Shop";
import { RequireSignIn } from "./components/RequireSignIn";
import { CartPage } from "./pages/Cart";
import { OrdersPage } from "./pages/Orders";
import { OrderDetailPage } from "./pages/OrderDetail";
import { NotFound } from "./pages/NotFound";
import { AdminLayout } from "./pages/admin/AdminLayout";
import { AdminOrders } from "./pages/admin/AdminOrders";
import { AdminOrder } from "./pages/admin/AdminOrder";
import { AdminProducts } from "./pages/admin/AdminProducts";
import { AdminSystem } from "./pages/admin/AdminSystem";

export function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Landing />} />
        <Route path="shop" element={<Shop />} />
        <Route path="cart" element={<RequireSignIn><CartPage /></RequireSignIn>} />
        <Route path="orders" element={<RequireSignIn><OrdersPage /></RequireSignIn>} />
        <Route path="orders/:id" element={<RequireSignIn><OrderDetailPage /></RequireSignIn>} />
        <Route path="admin" element={<AdminLayout />}>
          <Route index element={<AdminOrders />} />
          <Route path="orders/:id" element={<AdminOrder />} />
          <Route path="products" element={<AdminProducts />} />
          <Route path="system" element={<AdminSystem />} />
        </Route>
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
