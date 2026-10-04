import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	// Vite only exposes VITE_* by default. The browser bridge reads these
	// public, build-time values through import.meta.env.
	envPrefix: ['VITE_', 'PUBLIC_FLARE_'],
	server: {
		port: 5174,
		proxy: {
			'/api': { target: 'http://localhost:8080', changeOrigin: true },
			'/media': { target: 'http://localhost:8080', changeOrigin: true }
		}
	}
});
