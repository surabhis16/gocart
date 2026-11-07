import { Route, Routes } from "react-router-dom";
import { Layout } from "./components/Layout";
import { Shop } from "./pages/Shop";
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
        <Route index element={<Shop />} />
        <Route path="cart" element={<CartPage />} />
        <Route path="orders" element={<OrdersPage />} />
        <Route path="orders/:id" element={<OrderDetailPage />} />
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
