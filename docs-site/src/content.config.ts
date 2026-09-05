import { defineCollection, z } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

export const collections = {
	docs: defineCollection({
		loader: docsLoader(),
		schema: docsSchema({
			extend: z.object({
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
