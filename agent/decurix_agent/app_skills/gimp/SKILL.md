---
name: gimp
description: GIMP, this desktop's image editor (gui open photoshop opens it) - resizing, cropping, exporting JPEG and PNG
applies-to: gimp, photoshop, image editor
---

Opening
- gui open FILE opens an image in GIMP (gui open photoshop starts GIMP). Several images: open them one at a time,
  each in its own window, or File > Open (Ctrl+O) and type the whole path into the "Location" box (Ctrl+L shows it).

Common tasks
- Resize: Image > Scale Image... In the dialog, type the new Width in its field; with the chain beside it linked, the
  Height follows and keeps the proportions - read the Height back, then press "Scale".
- Crop to a size: Image > Canvas Size... or the Crop tool; for an exact area use Image > Crop to Selection after
  Select > All and Select > ... as the task needs.
- Undo a slip at once with Ctrl+Z.

Saving
- GIMP's Save (Ctrl+S) writes only its own .xcf. A JPEG or PNG is made with File > Export As... (Shift+Ctrl+E): type
  the whole path with the extension in "Name" (~/work/out/image.jpg), press "Export", then "Export" again in the
  quality dialog that follows.
- A folder that does not exist yet: make it first in the shell (mkdir -p ~/work/out).
- Check each file afterwards: office read FILE says its format and size in pixels.
- Closing an image that was only exported asks to save the .xcf: "Discard Changes" closes it.
