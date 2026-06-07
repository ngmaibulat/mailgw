# Step 1: Use an official Node.js runtime as a parent image
FROM node:26-alpine

# Step 2: Set the working directory inside the container
WORKDIR /opt/logservice

# Step 3: Copy package.json and package-lock.json (or yarn.lock) files
COPY package*.json ./

# Step 4: Install your app dependencies
RUN npm install

# If you're building for production
# RUN npm ci --only=production

# Step 5: Bundle your app's source code inside the Docker image
COPY . .

# Step 6: Your app binds to port 3000 so you'll use the EXPOSE instruction to have it mapped by the docker daemon
EXPOSE 3000

# Step 7: Define the command to run your app
CMD [ "npm", "start" ]
