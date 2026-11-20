# Map

The Map page shows all nodes on an interactive map, color-coded by role.

[Screenshot: map with colored markers and controls panel]

## Map style and overlays

The layers button on the map (top-left) chooses what the map underneath the nodes looks like. The same picker is on the [Live](live.md) page (top-right) and the Repeater Planner.

- **Auto** (the default) follows the site theme: a light map in light mode, a dark map in dark mode.
- Or pick a specific map. They are grouped by kind:
  - **Street**: road maps, including gray and dark styles that keep the node markers easy to read.
  - **Topographic**: terrain and contour maps.
  - **Satellite**: aerial imagery. Satellite maps look the same in light and dark mode.
- **Overlays** are checkboxes drawn on top of whichever map you chose:
  - **Hillshade**: shaded relief, so hills and valleys stand out. Useful with any map when you are judging radio line of sight.
  - **Place labels**: town and road names, mainly for satellite imagery, which has none.

Your choice is saved in your browser: the picker opens on it after a reload and on the Live and Repeater Planner pages too. The small maps on node pages, Analytics and Home show it whenever it suits the current theme (a satellite map always does; a light map is not used in dark mode), and otherwise the theme's map. It is not shared with other visitors and not stored on the server. Pick **Auto** to go back to following the theme.

Which maps the picker lists is up to the site's operator: the newer providers are offered only once the operator turns them on.

Some maps in the Street group are vector maps, drawn by your browser rather than downloaded as pictures. They stay sharp at every zoom level. If your browser can't draw them (no WebGL), or they fail to load within 15 seconds, the page shows the regular map for the current theme instead. The small maps on node pages, Analytics and Home always use a regular map: there, a vector choice shows the theme's default map.

Which maps appear in the list is up to the site operator, who can turn groups of maps on and off and add their own tile server. See [Map Tile Providers](../deployment.md#map-tile-providers).

### Tropospheric ducting

When the site operator has turned it on, the layers button has an extra **Tropospheric ducting** checkbox under Overlays (on the Map, Live and Repeater Planner pages). If you do not see it, this site does not offer it.

Ducting is when a layer of air traps radio signals and carries them far beyond the normal range, so a distant node may suddenly be heard. The overlay shows a forecast as colour bands over the area around the mesh. The colour shows the mean refractivity gradient over the lowest 1.5 km of the air: how much the air's refractivity drops per kilometre of height, averaged from the ground up to 1.5 km (in N units per km). The faster it drops, the more signals bend back toward the ground and the further they travel. Normal air drops by about 40 N/km. Each colour is one range of the gradient, not a point on a continuous scale: a whole band has the same colour and the colour changes at the band's edge, so you can read the strength off the legend. The band edges are at -44, -48, -54, -58, -62, -70, -80 and -90 N/km, with no colour above -44. The thresholds were tuned empirically on archived forecast data across several regions.

Colour means more bending than usual, not a confirmed duct. Warm, humid air bends signals more than dry air even without an inversion, so it shows as above normal.

- **No colour**: normal propagation (above -44 N/km).
- **Purple, blue and cyan**: above normal (-44 to -48, -48 to -54 and -54 to -58 N/km). Signals bend a little more than usual and may reach a little further.
- **Green, yellow and orange**: high (-58 to -62, -62 to -70 and -70 to -80 N/km). Signals bend noticeably more; the further along the scale, the stronger the effect.
- **Red and pink**: very high (-80 to -90, and -90 N/km and beyond).
- A **dashed outline** marks a grid cell where the model finds a trapping layer that may carry this site's LoRa frequency over long distances: its lowest trapped frequency is at most the frequency the operator set for line of sight (`los.frequencyMhz`), or, when the server has none, at most 915 MHz (so 868 or 915 MHz LoRa may be carried). The legend names the frequency it judges. The outline comes from the layers found in the model, not from the colour, so it can sit on any band, or on no colour at all. Treat it as "possible", never "will".
- With the Open-Meteo source, a faint **dotted line** marks the edge of the forecast area; the global grid has no edge. There is no forecast beyond it, and the colours fade out towards it, so a blank map outside the line means "not covered", not "no ducting".
- Areas without forecast data are not drawn; the field fades out at their edge.

#### Time slider and history

On sites that use the global grid (the default), the slider is labelled **Time** and covers the past days the operator keeps (7 by default) and the forecast ahead (48 hours by default), in 3-hour steps. The label under it says what you are looking at: a time after now ends in "forecast"; a time before now ends in "analysis, 6 h ago" (or days ago). Past times show the weather model's analysis or short-range forecast for that time, not measurements. The map shows the whole world, but each view loads only the part you are looking at, and zooming out makes the cells coarser (from 0.5 degrees up to 8). A view that crosses the 180 degree meridian shows only one side of it.

To share a time, copy the page address: `#/map?duct=1&ductT=<unix time>` opens the map with the overlay on at that time. Without `ductT` it opens at the current time.

#### Observed long links

With the overlay on, the map also draws lines for **observed long links**: a node heard directly by an observer, with no relay in between (a zero-hop advert), from farther away than the radio normally reaches. The cut-off is the radio horizon for the two antenna heights, `4.12 x (sqrt(h1) + sqrt(h2))` km with heights in metres, and links over 1,000 km are dropped as probably wrong positions. Heights are the ones users entered for their repeaters where the site has them, otherwise the site's default antenna height, so a link can be drawn or left out because of a guessed height.

Only links heard within 90 minutes of the selected time are drawn, so moving the slider moves the set. Click a line for the two nodes, the distance, the horizon, and the recent receptions with signal strength.

- A **solid line in the highlight colour** was heard while the model shows a duct that can carry this site's LoRa frequency along at least half of the path.
- A **solid line in the other colour** was heard with less or no modelled duct along the path.
- A **dashed line** has no model data for that time.

The legend adds a one-line summary for the period, for example "Last 7 days: 40 direct receptions beyond the radio horizon on 9 node pairs. 30% were during a modelled duct along the path; the same paths were ducted 12% of the time overall." Read the two percentages together: the second is how often those paths are ducted in the model at all, so the first is only interesting if it is clearly higher. It is a comparison of model output with receptions, not proof of cause. A reception outside a modelled duct is common, because the model is coarse (see below), and long links also come from wrong positions, high sites and line of sight over water.

Hidden and blacklisted nodes and observers never appear. Positions are self-reported. The colours of the bands, outline, edge and links can be changed in the customizer under **Ducting overlay**.

The legend shows the same bands as a bar, labelled Normal, Above normal, High and Very high. Use the time slider to move through the forecast (see below for history). Click anywhere in the field for the details of that grid cell: the gradient over the lowest 1.5 km, the type of layer found in the model (normal, super-refraction, elevated duct or surface-based duct), base and top of the layer (metres above ground), strength, the lowest trapped frequency ("≈ … MHz (model estimate)", to two significant figures) and a LoRa verdict for each frequency judged (this site's, or 868 and 915 MHz separately): "LoRa … MHz: possibly trapped (model estimate)" or "not trapped by this layer". The verdict compares the rounded figure shown, so within a few MHz of the frequency the answer is decided by rounding, not by the data. For a duct, the popup also notes that the model's levels are about 200 m apart, so thinner layers are not resolved. An elevated duct is a layer above the ground and mostly helps nodes that sit inside it, such as repeaters on high sites; a surface-based duct reaches the ground. The legend also shows the weather model, the data attribution and when the data was last updated.

It is an indicator only, and the LoRa verdict is an estimate. The model's levels are about 200-250 m apart, so the ducts it can see are thick ones. The thin ducts that most often carry 868 MHz, for example evaporation ducts over the sea, are mostly invisible to it. Each grid cell also covers tens of kilometres (0.5 degrees is about 55 km north to south): the band edges are interpolated between cell centres and show no detail finer than that. A clear map does not mean no long-distance reception, and a coloured or outlined area does not promise it. See also [Why did a node far away hear my packet?](faq.md#12-why-did-a-node-far-away-hear-my-packet)

## Marker shapes and colors

Each node role has a distinct shape and color:

| Role | Shape | Default Color |
|------|-------|---------------|
| Repeater | Diamond | Red |
| Companion | Circle | Blue |
| Room | Square | Green |
| Sensor | Triangle | Orange |
| Observer | Star | Purple |

Stale nodes (not heard recently) appear faded.

## Hash labels

Repeaters can display their short mesh hash ID instead of a plain marker. Toggle **Hash Labels** in the map controls to switch between icon markers and hash-labeled markers.

## Map controls

Open the controls panel with the ⚙️ button (top-right corner).

### Node types

Check or uncheck roles to show/hide them on the map. All roles are visible by default.

### Byte size filter

Filter nodes by packet size category: All, Small, Medium, Large.

### Status filter

Show only active, degraded, or silent nodes.

### Last heard filter

Limit the map to nodes heard within a time window (e.g., 24h, 7d, 30d).

### Clustering

Enable clustering to group nearby nodes into cluster bubbles. Zoom in to expand clusters.

### Neighbor filter

Select a reference node to highlight only its direct neighbors.

## Show Route

Click a node marker, then click **Show Route** in the popup to see the paths packets take to reach that node. Routes are drawn as lines between nodes.

## Popups

Click any marker to see:

- Node name and role
- Public key
- Last seen timestamp
- Link to the full node detail page

## Tips

- Zoom in on dense areas to see individual nodes
- Use the role checkboxes to isolate repeaters and understand coverage
- The neighbor filter is great for seeing which nodes can directly hear each other
- Node colors are [customizable](customization.md) in the theme settings
- Turn on the **Hillshade** overlay to see which nodes have high ground between them
- A gray street map makes colored markers stand out on a busy mesh
