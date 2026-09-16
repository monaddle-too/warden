import type { Metadata } from 'next';
import './globals.css';
export const metadata: Metadata = {
  title: 'OCSF Explorer',
  description:
    'Durable OCSF event ingestion, batch review, and security event investigation.',
};
export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
