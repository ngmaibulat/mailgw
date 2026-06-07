# Step 1: Use an official Node.js runtime as a parent image
FROM node:26-alpine

# Step 2: Set the working directory inside the container
WORKDIR /opt/db-migrator

# Step 3: Copy package.json and package-lock.json (or yarn.lock) files
COPY containers/db-migrator/package.json ./
COPY config ./config
COPY migrations ./migrations
COPY models ./models

# Step 4: Install your app dependencies
RUN npm install

# Step 7: Define the command to run your app
CMD [ "npx", "sequelize-cli", "db:migrate" ]
