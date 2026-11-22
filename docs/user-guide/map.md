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

#### Timeline and history

On sites that use the global grid (the default), the timeline is labelled **Time** and covers the past days the operator keeps (7 by default) and the forecast ahead (48 hours by default), in 3-hour steps; the dashed line on the strip is now, with the past to its left and the forecast to its right. The line above the strip says what you are looking at: a time after now ends in "forecast"; a time before now ends in "analysis, 6 h ago" (or days ago). **Now** goes back to the latest step at or before the current time. Past times show the weather model's analysis or short-range forecast for that time, not measurements. The map shows the whole world, but each view loads only the part you are looking at, and zooming out makes the cells coarser (from 0.5 degrees up to 8). A view that crosses the 180 degree meridian shows only one side of it.

To share a time, copy the page address: picking a time on the timeline writes it there, and `#/map?duct=1&ductT=<unix time>` opens the map with the overlay on at that time, with the strip at it. Without `ductT` it opens at the current time; going back with **Now** takes `ductT` out of the address.

#### Observed long links

With the overlay on, the map also draws lines for **observed long links**: two nodes that hear each other directly although there is no line of sight between them. Two kinds of evidence count, and only when the nodes are certain: a node heard by an observer with no relay in between (a zero-hop advert), and two repeaters in a relayed path whose short IDs each match exactly one node (origin to first relay, and relay to relay). Clicking a line says which kind each reception was; relayed ones have no signal strength.

"No line of sight" is decided from terrain when the site has line-of-sight terrain turned on, otherwise from the antennas' heights alone (the radio horizon). Heights are the ones users entered for their repeaters where the site has them, otherwise the site's default antenna height. Links over 1,000 km are dropped as probably wrong positions. A pair the site has not checked yet is left out and counted in the legend as "not checked yet".

Only links heard within 90 minutes of the selected time are drawn, so moving through the timeline moves the set. Click a line for the two nodes, the distance, and the recent receptions (one per 3-hour step) with the modelled gradient along the path.

- A **solid line in the highlight colour**: half or more of the path was in the High band (-62 N/km or stronger) in the model.
- A **solid line in the other colour**: less of the path was in the High band.
- A **dashed line**: no model data for that time.

The legend adds a summary for the period, for example "Last 7 days: 40 receptions on the 9 longest node pairs without terrain line of sight. Along their paths the modelled refractivity gradient averaged -45 N/km when heard, against -44 N/km for the same paths over the whole period". Read each pair of numbers together: the second is the same paths at every time in the period, so the first only matters if it is clearly more negative.

Below it, a table gives **how often the pairs were heard, by path gradient**: for each band of the modelled gradient along the path, the share of 3-hour steps in which the pair was heard (n is the number of pair-steps). If ducting matters, the share would rise towards the High band. The second column is for intermittent pairs only, those heard in some but not nearly all of their steps. This is the column to look at, because a pair that is heard almost every time is heard whatever the weather and says nothing about it.

How to read it honestly:

- A flat table means no visible link between the modelled gradient and being heard. A rising one is a hint, not proof.
- The model has 0.5 degree cells and levels about 200 m apart, so thin ducts are invisible. A low share in the High band does not mean no duct.
- Positions are self-reported; a wrong position makes a link look long.
- Where antennas are higher than the default height, "no line of sight" can be wrong, and where the site has no terrain data it ignores hills. Either way the list may contain pairs that did have line of sight, or miss some that did not.

Hidden and blacklisted nodes and observers never appear. The colours of the bands, outline, edge and links can be changed in the customizer under **Ducting overlay**.

The legend shows the same bands as a bar, labelled Normal, Above normal, High and Very high. Pick the time on the timeline under the panel's title (**Time** on the global grid, see above; **Forecast time** with the Open-Meteo source): the time under the centre marker is the one shown, with the date and hour written above it. Scroll or drag the strip (a mouse wheel scrolls it too), click a time, use ◀ and ▶ to step to the previous or next time, or focus the strip and use the arrow keys (Page Up / Page Down jump a day, Home / End go to the first and last time). **Now** jumps back to the current time. The chevron in the panel's title bar collapses the legend, observed long links included (the timeline stays), and on a larger screen you can drag the panel by its grip to anywhere on the map (**Reset** puts it back in the corner); both are remembered on this device. Click anywhere in the field for the details of that grid cell: the gradient over the lowest 1.5 km, the type of layer found in the model (normal, super-refraction, elevated duct or surface-based duct), base and top of the layer (metres above ground), strength, the lowest trapped frequency ("≈ … MHz (model estimate)", to two significant figures) and a LoRa verdict for each frequency judged (this site's, or 868 and 915 MHz separately): "LoRa … MHz: possibly trapped (model estimate)" or "not trapped by this layer". The verdict compares the rounded figure shown, so within a few MHz of the frequency the answer is decided by rounding, not by the data. For a duct, the popup also notes that the model's levels are about 200 m apart, so thinner layers are not resolved. An elevated duct is a layer above the ground and mostly helps nodes that sit inside it, such as repeaters on high sites; a surface-based duct reaches the ground. The legend also shows the weather model, the data attribution and when the data was last updated.

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
