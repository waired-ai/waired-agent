import { defineCollection, z } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

export const collections = {
	docs: defineCollection({
		loader: docsLoader(),
		schema: docsSchema({
			extend: z.object({
				// Set on `ja/` pages only: the digest of the English page this
				// translation was made from. `scripts/i18n-sync.mjs` writes it
				// and CI compares it, so an English edit cannot quietly leave
				// the Japanese page describing old behaviour. A page whose
				// hash no longer matches renders a "may be out of date" notice
				// (src/components/PageTitle.astro) as a second line of defence.
				sourceHash: z.string().optional(),
				// Per-page header block (the "who this is for / what you need /
				// how long" convention). Rendered by src/components/PageTitle.
				meta: z
					.object({
						audience: z.string().optional(),
						needs: z.string().optional(),
						time: z.string().optional(),
					})
					.optional(),
			}),
		}),
	}),
};
